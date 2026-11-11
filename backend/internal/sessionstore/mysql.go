package sessionstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"backend/internal/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SessionEntry is a row of session_entries, the table of all the kinds of
// entry. Its definition is copied into migration 20260101000002 (as
// sessionEntryV1); a change here needs a new migration, and
// TestSessionEntriesMatchModel in the database package fails without one.
//
// The key columns are case-sensitive on MySQL (the migration sets
// utf8mb4_0900_bin). ExpiresAt is Unix milliseconds. Data is the JSON of an OIDC
// state, or the block time of a user block in Unix milliseconds.
type SessionEntry struct {
	EntryKey  string `gorm:"column:entry_key;primaryKey;size:255"`
	Kind      string `gorm:"column:kind;primaryKey;size:20"`
	Data      string `gorm:"column:data;type:text"`
	ExpiresAt int64  `gorm:"column:expires_at;not null;index:idx_session_entries_expires_at"`
}

// TableName is the name of the table.
func (SessionEntry) TableName() string { return "session_entries" }

// sessionBlockBatch is the number of rows of one BlockSessions upsert. It
// keeps a statement (4 values per row) well below max_allowed_packet.
const sessionBlockBatch = 500

// mysqlStore is the SessionStore on the shared database.
type mysqlStore struct {
	db      *gorm.DB
	now     func() time.Time
	janitor *janitor
}

// NewMySQL returns the store on db and starts its cleanup goroutine, which
// Stop ends. The table session_entries must exist (migration 20260101000002).
func NewMySQL(db *gorm.DB) SessionStore {
	return newMySQL(db, time.Now, defaultCleanupEvery)
}

func newMySQL(db *gorm.DB, now func() time.Time, cleanupEvery time.Duration) *mysqlStore {
	s := &mysqlStore{db: db, now: now}
	s.janitor = startJanitor(cleanupEvery, s.Cleanup)
	return s
}

func (s *mysqlStore) nowMs() int64 { return s.now().UnixMilli() }

// write runs one write statement and runs it again after a deadlock or lock
// wait timeout (models.RetryOnLock). The store never runs inside an outer
// transaction, so the retry is safe.
func (s *mysqlStore) write(ctx context.Context, fn func(db *gorm.DB) error) error {
	return models.RetryOnLock(ctx, func() error { return fn(s.db.WithContext(ctx)) })
}

// upsert writes the rows. When a row exists it keeps the later expiry, so a
// short block never shortens a long one. All rows share one expiry. With
// blockedAtMs > 0 (a user block) an existing row also keeps the later block
// time in data, so the cut never moves back; the rows carry it as Data.
func (s *mysqlStore) upsert(ctx context.Context, rows []SessionEntry, blockedAtMs int64) error {
	// The SQL fragments are constants chosen by dialect, never input.
	greatest, intType, textType := "GREATEST", "SIGNED", "CHAR"
	if s.db.Dialector.Name() == "sqlite" {
		greatest, intType, textType = "MAX", "INTEGER", "TEXT" // the unit tests run on SQLite
	}
	set := map[string]any{"expires_at": gorm.Expr(greatest+"(expires_at, ?)", rows[0].ExpiresAt)}
	if blockedAtMs > 0 {
		set["data"] = gorm.Expr("CAST("+greatest+"(CAST(data AS "+intType+"), ?) AS "+textType+")", blockedAtMs)
	}
	return s.write(ctx, func(db *gorm.DB) error {
		return db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "entry_key"}, {Name: "kind"}},
			DoUpdates: clause.Assignments(set),
		}).Create(&rows).Error
	})
}

// active reports whether an unexpired row of the kind exists.
func (s *mysqlStore) active(ctx context.Context, key, kind string) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&SessionEntry{}).
		Where("entry_key = ? AND kind = ? AND expires_at > ?", key, kind, s.nowMs()).
		Count(&n).Error
	if err != nil {
		return false, fmt.Errorf("read %s: %w", kind, err)
	}
	return n > 0, nil
}

func (s *mysqlStore) block(ctx context.Context, key, kind string, until time.Time) error {
	if err := preflight(ctx, key); err != nil {
		return err
	}
	if err := s.upsert(ctx, []SessionEntry{{EntryKey: key, Kind: kind, ExpiresAt: expiryMs(until)}}, 0); err != nil {
		return fmt.Errorf("write %s: %w", kind, err)
	}
	return nil
}

func (s *mysqlStore) BlockToken(ctx context.Context, jti string, expiresAt time.Time) error {
	return s.block(ctx, jti, kindTokenBlock, expiresAt)
}

func (s *mysqlStore) IsTokenBlocked(ctx context.Context, jti string) (bool, error) {
	if err := preflight(ctx, jti); err != nil {
		return false, err
	}
	return s.active(ctx, jti, kindTokenBlock)
}

// BlockUser writes the block time (now, in milliseconds) in Data. A second
// block keeps the later block time and the later expiry, so the cut never
// moves back.
func (s *mysqlStore) BlockUser(ctx context.Context, userID string, until time.Time) error {
	if err := preflight(ctx, userID); err != nil {
		return err
	}
	blockedAt := s.nowMs()
	row := SessionEntry{
		EntryKey:  userID,
		Kind:      kindUserBlock,
		Data:      strconv.FormatInt(blockedAt, 10),
		ExpiresAt: expiryMs(until),
	}
	if err := s.upsert(ctx, []SessionEntry{row}, max(blockedAt, 1)); err != nil {
		return fmt.Errorf("write %s: %w", kindUserBlock, err)
	}
	return nil
}

// IsUserBlocked reads the block time of an unexpired user block. A row whose
// block time is not a number blocks every token.
func (s *mysqlStore) IsUserBlocked(ctx context.Context, userID string, issuedAt time.Time) (bool, error) {
	if err := preflight(ctx, userID); err != nil {
		return false, err
	}
	var rows []SessionEntry
	err := s.db.WithContext(ctx).Select("data").
		Where("entry_key = ? AND kind = ? AND expires_at > ?", userID, kindUserBlock, s.nowMs()).
		Limit(1).Find(&rows).Error
	if err != nil {
		return false, fmt.Errorf("read %s: %w", kindUserBlock, err)
	}
	if len(rows) == 0 {
		return false, nil
	}
	blockedAt, err := strconv.ParseInt(rows[0].Data, 10, 64)
	if err != nil {
		blockedAt = 0
	}
	return userBlockApplies(blockedAt, issuedAt), nil
}

func (s *mysqlStore) UnblockUser(ctx context.Context, userID string) error {
	if err := preflight(ctx, userID); err != nil {
		return err
	}
	err := s.write(ctx, func(db *gorm.DB) error {
		return db.Where("entry_key = ? AND kind = ?", userID, kindUserBlock).Delete(&SessionEntry{}).Error
	})
	if err != nil {
		return fmt.Errorf("delete %s: %w", kindUserBlock, err)
	}
	return nil
}

func (s *mysqlStore) BlockSession(ctx context.Context, sessionID string, until time.Time) error {
	return s.block(ctx, sessionID, kindSessionBlock, until)
}

// BlockSessions writes the session blocks with one multi-row upsert per batch
// of sessionBlockBatch rows. A session ID that appears twice is written once.
//
// The batches are separate statements, not one transaction: when a later
// batch fails, the earlier ones stay written and the error says how many
// sessions were blocked. That is intended. A block only ever blocks more, a
// second write of the same block is harmless, and a rollback would leave every
// session of the call unblocked instead of some.
func (s *mysqlStore) BlockSessions(ctx context.Context, sessionIDs []string, until time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkKeys(sessionIDs); err != nil {
		return err
	}
	ids := dedupe(sessionIDs)
	expires := expiryMs(until)
	for start := 0; start < len(ids); start += sessionBlockBatch {
		batch := make([]SessionEntry, 0, sessionBlockBatch)
		for _, id := range ids[start:min(start+sessionBlockBatch, len(ids))] {
			batch = append(batch, SessionEntry{EntryKey: id, Kind: kindSessionBlock, ExpiresAt: expires})
		}
		if err := s.upsert(ctx, batch, 0); err != nil {
			return fmt.Errorf("blocked %d of %d sessions: %w", start, len(ids), err)
		}
	}
	return nil
}

func (s *mysqlStore) IsSessionBlocked(ctx context.Context, sessionID string) (bool, error) {
	if err := preflight(ctx, sessionID); err != nil {
		return false, err
	}
	return s.active(ctx, sessionID, kindSessionBlock)
}

// SaveOIDCState replaces an existing row of the same state.
func (s *mysqlStore) SaveOIDCState(ctx context.Context, state string, data OIDCStateData, ttl time.Duration) error {
	if err := preflight(ctx, state); err != nil {
		return err
	}
	raw, err := marshalState(data, ttl)
	if err != nil {
		return err
	}
	row := SessionEntry{EntryKey: stateKey(state), Kind: kindOIDCState, Data: string(raw), ExpiresAt: expiryMs(s.now().Add(ttl))}
	err = s.write(ctx, func(db *gorm.DB) error {
		return db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "entry_key"}, {Name: "kind"}},
			DoUpdates: clause.AssignmentColumns([]string{"data", "expires_at"}),
		}).Create(&row).Error
	})
	if err != nil {
		return fmt.Errorf("write %s: %w", kindOIDCState, err)
	}
	return nil
}

// ConsumeOIDCState reads the unexpired row and then deletes it with the same
// conditions. The delete decides: of several consumers that read the row, only
// one deletes it (the row lock makes the others wait and then find nothing),
// so only that one gets RowsAffected 1 and the data. The others get (nil,
// nil). No transaction is needed, so there is no lock held between the two
// statements.
func (s *mysqlStore) ConsumeOIDCState(ctx context.Context, state string) (*OIDCStateData, error) {
	if err := preflight(ctx, state); err != nil {
		return nil, err
	}
	now := s.nowMs()
	key := stateKey(state)
	const where = "entry_key = ? AND kind = ? AND expires_at > ?"

	var rows []SessionEntry
	if err := s.db.WithContext(ctx).Where(where, key, kindOIDCState, now).Limit(1).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("read %s: %w", kindOIDCState, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	var deleted int64
	err := s.write(ctx, func(db *gorm.DB) error {
		res := db.Where(where, key, kindOIDCState, now).Delete(&SessionEntry{})
		deleted = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return nil, fmt.Errorf("delete %s: %w", kindOIDCState, err)
	}
	if deleted == 0 {
		return nil, nil // another consumer was first
	}

	var data OIDCStateData
	if err := json.Unmarshal([]byte(rows[0].Data), &data); err != nil {
		return nil, fmt.Errorf("decode %s: %w", kindOIDCState, err)
	}
	return &data, nil
}

// Cleanup deletes the rows that have expired. The index on expires_at serves
// the delete.
func (s *mysqlStore) Cleanup(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.nowMs()
	err := s.write(ctx, func(db *gorm.DB) error {
		return db.Where("expires_at <= ?", now).Delete(&SessionEntry{}).Error
	})
	if err != nil {
		return fmt.Errorf("delete expired entries: %w", err)
	}
	return nil
}

func (s *mysqlStore) Stop() { s.janitor.stop() }
