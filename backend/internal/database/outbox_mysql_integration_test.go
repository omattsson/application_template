//go:build integration

package database

import (
	"context"
	"sync"
	"testing"
	"time"

	"backend/internal/models"
	"backend/internal/outbox"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingSender collects the delivered messages.
type recordingSender struct {
	msgs []string
	mu   sync.Mutex
}

func (s *recordingSender) Broadcast(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, string(b))
}

func (s *recordingSender) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

// TestDatabaseOutboxTable: the migrated column types and the index on
// MySQL, and the repository reads and cleanup with the server clock.
func TestDatabaseOutboxTable(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	repo := NewGORMOutboxRepository(db.DB)
	ctx := context.Background()

	for col, want := range map[string]string{
		"id": "bigint unsigned", "created_at": "datetime(3)", "event_type": "varchar(64)", "message": "text",
	} {
		var got string
		require.NoError(t, db.Raw("SELECT COLUMN_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'outbox_events' AND column_name = ?", col).Scan(&got).Error)
		assert.Equal(t, want, got, col)
	}
	var idx int64
	require.NoError(t, db.Raw("SELECT COUNT(1) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'outbox_events' AND index_name = 'idx_outbox_events_created_at'").Scan(&idx).Error)
	assert.Equal(t, int64(1), idx)

	before, err := repo.MaxID(ctx)
	require.NoError(t, err)
	require.NoError(t, InsertOutboxEvent(db.DB, "it.test", []byte(`{"type":"it.test","payload":{}}`)))
	require.NoError(t, InsertOutboxEvent(db.DB, "it.test", []byte(`{"type":"it.test","payload":{}}`)))
	t.Cleanup(func() { _ = db.Exec("DELETE FROM outbox_events WHERE event_type = 'it.test'").Error })

	rows, err := repo.ListAfter(ctx, before, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Less(t, rows[0].ID, rows[1].ID)

	age, ok, err := repo.Age(ctx, rows[0].ID)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Less(t, age, 5*time.Second, "created_at is the server's UTC time")
	assert.GreaterOrEqual(t, age, time.Duration(0))

	require.NoError(t, db.Exec("UPDATE outbox_events SET created_at = UTC_TIMESTAMP(3) - INTERVAL 2 HOUR WHERE id = ?", rows[0].ID).Error)
	n, err := repo.DeleteOlderThan(ctx, time.Hour, 100)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1))
	byIDs, err := repo.ListByIDs(ctx, []uint64{rows[0].ID, rows[1].ID})
	require.NoError(t, err)
	require.Len(t, byIDs, 1, "only the old row is deleted")
	assert.Equal(t, rows[1].ID, byIDs[0].ID)
}

// TestDatabaseOutboxOutOfOrderCommit is the central correctness point: two
// transactions take consecutive auto increment ids and commit in reverse
// order, so the higher id is visible first. The poller must deliver both.
func TestDatabaseOutboxOutOfOrderCommit(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	t.Cleanup(func() { _ = db.Exec("DELETE FROM outbox_events WHERE event_type = 'it.order'").Error })
	ctx := context.Background()

	sender := &recordingSender{}
	poller := outbox.NewPoller(outbox.Config{
		Repo:      NewGORMOutboxRepository(db.DB),
		Sender:    sender,
		Interval:  10 * time.Millisecond,
		GapWindow: 30 * time.Second,
	})
	require.NoError(t, poller.Poll(ctx)) // the cursor starts at the table maximum

	first := db.Begin()
	require.NoError(t, first.Error)
	second := db.Begin()
	require.NoError(t, second.Error)
	// The first transaction takes the lower id, the second the higher one.
	require.NoError(t, InsertOutboxEvent(first, "it.order", []byte(`{"type":"it.order","payload":{"n":"first"}}`)))
	require.NoError(t, InsertOutboxEvent(second, "it.order", []byte(`{"type":"it.order","payload":{"n":"second"}}`)))

	require.NoError(t, second.Commit().Error) // the higher id commits first
	require.NoError(t, poller.Poll(ctx))
	assert.Equal(t, []string{`{"type":"it.order","payload":{"n":"second"}}`}, sender.list(),
		"the higher id is visible alone: the lower id is still uncommitted")

	require.NoError(t, first.Commit().Error) // the lower id becomes visible later
	require.NoError(t, poller.Poll(ctx))
	assert.Equal(t, []string{
		`{"type":"it.order","payload":{"n":"second"}}`,
		`{"type":"it.order","payload":{"n":"first"}}`,
	}, sender.list(), "the late row is delivered, not skipped")

	require.NoError(t, poller.Poll(ctx))
	assert.Len(t, sender.list(), 2, "each event is delivered once")
}

// A rolled back change leaves no outbox row, on MySQL.
func TestDatabaseOutboxRollbackLeavesNoRow(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	repo := models.NewRepository(db.DB, models.WithChangeHook(outbox.ItemHook(InsertOutboxEvent)))
	outboxRepo := NewGORMOutboxRepository(db.DB)
	ctx := context.Background()

	before, err := outboxRepo.MaxID(ctx)
	require.NoError(t, err)

	// A stale update (no such item) and an invalid create write nothing.
	require.Error(t, repo.Update(ctx, &models.Item{Base: models.Base{ID: 4_000_000_000}, Name: "x", Price: 1, Version: 3}))
	require.Error(t, repo.Create(ctx, &models.Item{Name: "", Version: 1}))
	require.Error(t, repo.Delete(ctx, &models.Item{Base: models.Base{ID: 4_000_000_000}}))

	rows, err := outboxRepo.ListAfter(ctx, before, 10)
	require.NoError(t, err)
	assert.Empty(t, rows, "no row for a failed change")

	// A committed create writes exactly one.
	item := &models.Item{Name: "it-outbox", Price: 1, Version: 1}
	require.NoError(t, repo.Create(ctx, item))
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM items WHERE id = ?", item.ID).Error
		_ = db.Exec("DELETE FROM outbox_events WHERE id > ?", before).Error
	})
	rows, err = outboxRepo.ListAfter(ctx, before, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "item.created", rows[0].EventType)
}
