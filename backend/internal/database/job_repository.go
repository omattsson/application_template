package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// Compile-time interface check.
var _ models.JobRepository = (*GORMJobRepository)(nil)

// GORMJobRepository implements models.JobRepository with GORM. MySQL
// arbitrates between replicas: a claim is one transaction that locks the due
// rows with SELECT ... FOR UPDATE SKIP LOCKED, so two workers never take the
// same row and neither waits for the other, and every update that reports a
// result is fenced by the worker ID and the running status. Every time is
// written and compared with the database clock (UTC_TIMESTAMP(6)), so a clock
// difference between the replicas does not matter and no Go time value
// crosses the driver.
type GORMJobRepository struct {
	*BaseRepository
}

// NewGORMJobRepository creates a GORM-backed job repository. The table jobs
// must exist (migration 20261121000001). Its statements run on a session with
// a silent GORM logger: a failing statement would otherwise log its values,
// which include a payload that may hold sensitive data, and the pool logs a
// failure itself, through a logutil.Limiter.
func NewGORMJobRepository(db *gorm.DB) *GORMJobRepository {
	if db != nil && db.Logger != nil {
		db = db.Session(&gorm.Session{Logger: db.Logger.LogMode(logger.Silent)})
	}

	return &GORMJobRepository{BaseRepository: NewBaseRepository(db)}
}

// jobNowSQL is the current database time with microsecond precision
// (millisecond on SQLite, for the unit tests). It is a constant per dialect,
// never built from input.
func jobNowSQL(dialect string) string {
	if dialect == dialectMySQL {
		return "UTC_TIMESTAMP(6)"
	}

	return "strftime('%Y-%m-%d %H:%M:%f', 'now')"
}

// jobOffsetSQL is the database time plus the offset given as the one argument
// (jobOffsetArg).
func jobOffsetSQL(dialect string) string {
	if dialect == dialectMySQL {
		return "(UTC_TIMESTAMP(6) + INTERVAL ? MICROSECOND)"
	}

	return "strftime('%Y-%m-%d %H:%M:%f', 'now', ?)"
}

// jobOffsetArg is the argument of jobOffsetSQL for d (negative: the past).
func jobOffsetArg(dialect string, d time.Duration) any {
	return idemOffsetArg(dialect, d)
}

// jobAgeSQL is the age of the oldest row (MIN of run_at) in microseconds by
// the database clock; NULL for no row.
func jobAgeSQL(dialect string) string {
	if dialect == dialectMySQL {
		return "TIMESTAMPDIFF(MICROSECOND, MIN(run_at), UTC_TIMESTAMP(6))"
	}

	return "CAST((julianday('now') - julianday(MIN(run_at))) * 86400000000 AS INTEGER)"
}

// jobLockSkip adds FOR UPDATE SKIP LOCKED on MySQL. SQLite (the unit tests)
// has no row locks and runs one writer at a time.
func jobLockSkip(q *gorm.DB, dialect string) *gorm.DB {
	if dialect != dialectMySQL {
		return q
	}

	return q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate, Options: clause.LockingOptionsSkipLocked})
}

// truncateChars cuts s to at most n characters and replaces bytes that are not
// UTF-8 (a text of that kind is refused by a strict utf8mb4 column).
func truncateChars(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if utf8.RuneCountInString(s) <= n {
		return s
	}

	return string([]rune(s)[:n])
}

// Enqueue implements models.JobRepository.
func (r *GORMJobRepository) Enqueue(ctx context.Context, spec models.JobSpec) (uint64, error) {
	switch {
	case spec.Type == "" || len(spec.Type) > models.JobTypeMaxLen:
		return 0, validationError("job enqueue", errors.New("type must have 1 to 100 characters"))
	case len(spec.Payload) == 0 || len(spec.Payload) > models.JobPayloadMaxBytes:
		return 0, validationError("job enqueue", errors.New("payload must have 1 byte to 64 KiB"))
	case spec.MaxAttempts < 1 || spec.MaxAttempts > models.JobMaxAttemptsLimit:
		return 0, validationError("job enqueue", errors.New("max attempts must be from 1 to 50"))
	case spec.Delay < 0:
		return 0, validationError("job enqueue", errors.New("delay must not be negative"))
	}

	dialect := r.DB.Dialector.Name()
	now := jobNowSQL(dialect)
	// The expressions are constants per dialect; every value is an argument.
	stmt := "INSERT INTO jobs (type, payload, status, attempts, max_attempts, run_at, locked_by, last_error, created_at, updated_at) " +
		"VALUES (?, ?, 'pending', 0, ?, " + jobOffsetSQL(dialect) + ", '', '', " + now + ", " + now + ")"

	var id uint64

	err := r.Write(ctx, func(db *gorm.DB) error {
		id = 0

		res, err := db.Statement.ConnPool.ExecContext(ctx, stmt,
			spec.Type, string(spec.Payload), spec.MaxAttempts, jobOffsetArg(dialect, spec.Delay))
		if err != nil {
			return err
		}

		last, err := res.LastInsertId()
		if err != nil {
			return err
		}

		id = uint64(last) // #nosec G115 -- an auto increment id is positive

		return nil
	})
	if err != nil {
		return 0, repoError(ctx, "job enqueue", err)
	}

	return id, nil
}

// jobClaimRow is the columns that a claim reads.
type jobClaimRow struct {
	Type        string
	Payload     string
	ID          uint64
	Attempts    int32
	MaxAttempts int32
}

func (r *GORMJobRepository) selectClaimable(tx *gorm.DB, dialect, where string, types []string, limit int) ([]jobClaimRow, error) {
	var rows []jobClaimRow

	q := tx.Table("jobs").
		Select("id", "type", "payload", "attempts", "max_attempts").
		Where(where).
		Where("type IN ?", types).
		Order("run_at ASC").Order("id ASC").
		Limit(limit)

	return rows, jobLockSkip(q, dialect).Scan(&rows).Error
}

// Claim implements models.JobRepository.
//
// One transaction: the running rows whose lease passed come first (they are
// few, and a due backlog must not starve them), then the due pending rows,
// each locked with FOR UPDATE SKIP LOCKED in run_at, id order. A row of the
// first kind whose attempts are used up becomes dead and is not returned for
// running. Then one UPDATE per claimed row sets the lease, its worker and
// attempts + 1. The whole transaction runs again after a deadlock, so the
// result is reset at its start.
func (r *GORMJobRepository) Claim(ctx context.Context, workerIDs, types []string, lease time.Duration) (models.JobClaim, error) {
	if len(workerIDs) == 0 || len(types) == 0 {
		return models.JobClaim{}, nil
	}

	if lease <= 0 {
		return models.JobClaim{}, validationError("job claim", errors.New("lease must be positive"))
	}

	dialect := r.DB.Dialector.Name()
	now := jobNowSQL(dialect)

	var result models.JobClaim

	err := r.Tx(ctx, func(tx *gorm.DB) error {
		result = models.JobClaim{}

		expired, err := r.selectClaimable(tx, dialect, "status = 'running' AND locked_until < "+now, types, len(workerIDs))
		if err != nil {
			return err
		}

		var claimed []jobClaimRow

		for _, row := range expired {
			if row.Attempts < row.MaxAttempts {
				claimed = append(claimed, row)

				continue
			}

			msg := fmt.Sprintf("lease expired after %d attempts", row.Attempts)
			if err := tx.Exec("UPDATE jobs SET status = 'dead', last_error = ?, locked_by = '', locked_until = NULL, finished_at = "+now+", updated_at = "+now+
				" WHERE id = ? AND status = 'running'", msg, row.ID).Error; err != nil {
				return err
			}

			result.Expired = append(result.Expired, models.Job{ID: row.ID, Type: row.Type, Status: models.JobStatusDead, Attempts: row.Attempts, MaxAttempts: row.MaxAttempts, LastError: msg})
		}

		if free := len(workerIDs) - len(claimed); free > 0 {
			pending, err := r.selectClaimable(tx, dialect, "status = 'pending' AND run_at <= "+now, types, free)
			if err != nil {
				return err
			}

			claimed = append(claimed, pending...)
		}

		for i, row := range claimed {
			if err := tx.Exec("UPDATE jobs SET status = 'running', attempts = attempts + 1, locked_by = ?, locked_until = "+jobOffsetSQL(dialect)+", updated_at = "+now+
				" WHERE id = ?", workerIDs[i], jobOffsetArg(dialect, lease), row.ID).Error; err != nil {
				return err
			}

			result.Jobs = append(result.Jobs, models.Job{
				ID: row.ID, Type: row.Type, Payload: row.Payload, Status: models.JobStatusRunning,
				Attempts: row.Attempts + 1, MaxAttempts: row.MaxAttempts, LockedBy: workerIDs[i],
			})
		}

		return nil
	})
	if err != nil {
		return models.JobClaim{}, repoError(ctx, "job claim", err)
	}

	return result, nil
}

// fenced runs one UPDATE of the running job id held by workerID and reports
// whether it changed a row.
func (r *GORMJobRepository) fenced(ctx context.Context, op, set string, id uint64, workerID string, args ...any) (bool, error) {
	var affected int64

	// set is a constant of this file; the values are arguments.
	err := r.Write(ctx, func(db *gorm.DB) error {
		affected = 0

		all := append(append([]any{}, args...), id, workerID)
		res := db.Exec("UPDATE jobs SET "+set+" WHERE id = ? AND locked_by = ? AND status = 'running'", all...)
		affected = res.RowsAffected

		return res.Error
	})
	if err != nil {
		return false, repoError(ctx, op, err)
	}

	return affected > 0, nil
}

// Extend implements models.JobRepository.
func (r *GORMJobRepository) Extend(ctx context.Context, id uint64, workerID string, lease time.Duration) (bool, error) {
	if lease <= 0 {
		return false, validationError("job extend", errors.New("lease must be positive"))
	}

	dialect := r.DB.Dialector.Name()

	return r.fenced(ctx, "job extend", "locked_until = "+jobOffsetSQL(dialect)+", updated_at = "+jobNowSQL(dialect),
		id, workerID, jobOffsetArg(dialect, lease))
}

// Complete implements models.JobRepository.
func (r *GORMJobRepository) Complete(ctx context.Context, id uint64, workerID string) (bool, error) {
	now := jobNowSQL(r.DB.Dialector.Name())

	return r.fenced(ctx, "job complete",
		"status = 'succeeded', locked_by = '', locked_until = NULL, finished_at = "+now+", updated_at = "+now,
		id, workerID)
}

// Fail implements models.JobRepository.
func (r *GORMJobRepository) Fail(ctx context.Context, id uint64, workerID, lastError string, retryIn time.Duration, dead bool) (bool, error) {
	if retryIn < 0 {
		return false, validationError("job fail", errors.New("retry delay must not be negative"))
	}

	dialect := r.DB.Dialector.Name()
	now := jobNowSQL(dialect)
	lastError = truncateChars(lastError, models.JobLastErrorMaxChars)

	if dead {
		return r.fenced(ctx, "job fail",
			"status = 'dead', last_error = ?, locked_by = '', locked_until = NULL, finished_at = "+now+", updated_at = "+now,
			id, workerID, lastError)
	}

	return r.fenced(ctx, "job fail",
		"status = 'pending', last_error = ?, run_at = "+jobOffsetSQL(dialect)+", locked_by = '', locked_until = NULL, updated_at = "+now,
		id, workerID, lastError, jobOffsetArg(dialect, retryIn))
}

// Release implements models.JobRepository.
func (r *GORMJobRepository) Release(ctx context.Context, id uint64, workerID string) (bool, error) {
	now := jobNowSQL(r.DB.Dialector.Name())

	return r.fenced(ctx, "job release",
		"status = 'pending', attempts = CASE WHEN attempts > 0 THEN attempts - 1 ELSE 0 END, run_at = "+now+", locked_by = '', locked_until = NULL, updated_at = "+now,
		id, workerID)
}

// dbTime scans a DATETIME column on MySQL (a time.Time with parseTime, bytes
// without it) and on SQLite (the unit tests store text). The value is UTC.
type dbTime struct {
	Time  time.Time
	Valid bool
}

// Scan implements sql.Scanner.
func (t *dbTime) Scan(v any) error {
	*t = dbTime{}

	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		t.Time, t.Valid = x.UTC(), true

		return nil
	case []byte:
		return t.parse(string(x))
	case string:
		return t.parse(x)
	default:
		return fmt.Errorf("unsupported time value of type %T", v)
	}
}

func (t *dbTime) parse(s string) error {
	for _, layout := range []string{"2006-01-02 15:04:05.999999", "2006-01-02T15:04:05.999999Z07:00"} {
		if v, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			t.Time, t.Valid = v.UTC(), true

			return nil
		}
	}

	return fmt.Errorf("unsupported time text %q", s)
}

// Value implements driver.Valuer; GORM needs it to accept the type in a row
// struct. The repository never writes a dbTime.
func (t dbTime) Value() (driver.Value, error) {
	if !t.Valid {
		return nil, nil
	}

	return t.Time, nil
}

func (t dbTime) ptr() *time.Time {
	if !t.Valid {
		return nil
	}

	v := t.Time

	return &v
}

// jobListRow is the columns of a list page: everything but the payload.
type jobListRow struct {
	RunAt       dbTime
	CreatedAt   dbTime
	UpdatedAt   dbTime
	LockedUntil dbTime
	FinishedAt  dbTime
	Type        string
	Status      string
	LockedBy    string
	LastError   string
	ID          uint64
	Attempts    int32
	MaxAttempts int32
}

var (
	_ sql.Scanner   = (*dbTime)(nil)
	_ driver.Valuer = dbTime{}
)

const jobListColumns = "id, type, status, attempts, max_attempts, run_at, locked_by, locked_until, last_error, created_at, updated_at, finished_at"

// jobSortColumns maps a sort key (models.JobSortKeys) to its column. It is the
// only place where a key becomes SQL: a key that is not in it is refused.
var jobSortColumns = map[string]string{
	"created_at": "created_at",
	"run_at":     "run_at",
	"id":         "id",
}

// ListPaged implements models.JobRepository.
func (r *GORMJobRepository) ListPaged(ctx context.Context, filter models.JobFilter, limit, offset int) ([]models.Job, int64, error) {
	key := filter.SortBy
	if key == "" {
		key = "id"
	}

	col, ok := jobSortColumns[key]
	if !ok {
		return nil, 0, dberrors.NewDatabaseError("job list", fmt.Errorf("%w: sort key", dberrors.ErrValidation))
	}

	if filter.Status != "" && !models.ValidJobStatus(filter.Status) {
		return nil, 0, dberrors.NewDatabaseError("job list", fmt.Errorf("%w: status", dberrors.ErrValidation))
	}

	where := func(q *gorm.DB) *gorm.DB {
		if filter.Status != "" {
			q = q.Where("status = ?", filter.Status)
		}

		if filter.Type != "" {
			q = q.Where("type = ?", filter.Type)
		}

		return q
	}

	db := r.DB.WithContext(ctx)

	var total int64
	if err := where(db.Table("jobs")).Count(&total).Error; err != nil {
		return nil, 0, repoError(ctx, "job list", err)
	}

	q := where(db.Table("jobs")).Select(jobListColumns).
		Order(clause.OrderByColumn{Column: clause.Column{Name: col}, Desc: filter.SortDesc})
	if col != "id" {
		q = q.Order(clause.OrderByColumn{Column: clause.Column{Name: "id"}, Desc: filter.SortDesc})
	}

	if limit > 0 {
		q = q.Limit(limit)
		if offset > 0 {
			q = q.Offset(offset)
		}
	}

	var rows []jobListRow
	if err := q.Scan(&rows).Error; err != nil {
		return nil, 0, repoError(ctx, "job list", err)
	}

	jobs := make([]models.Job, 0, len(rows))
	for _, row := range rows {
		jobs = append(jobs, models.Job{
			ID: row.ID, Type: row.Type, Status: row.Status, Attempts: row.Attempts, MaxAttempts: row.MaxAttempts,
			RunAt: row.RunAt.Time, LockedBy: row.LockedBy, LockedUntil: row.LockedUntil.ptr(), LastError: row.LastError,
			CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time, FinishedAt: row.FinishedAt.ptr(),
		})
	}

	return jobs, total, nil
}

// Stats implements models.JobRepository. Two reads: the counts per status
// (an index-only scan of idx_jobs_claim) and the due pending jobs with the
// age of the oldest.
func (r *GORMJobRepository) Stats(ctx context.Context) (models.JobStats, error) {
	dialect := r.DB.Dialector.Name()
	db := r.DB.WithContext(ctx)

	var counts []struct {
		Status string
		N      int64
	}

	if err := db.Table("jobs").Select("status, COUNT(*) AS n").Group("status").Scan(&counts).Error; err != nil {
		return models.JobStats{}, repoError(ctx, "job stats", err)
	}

	var stats models.JobStats

	for _, c := range counts {
		switch c.Status {
		case models.JobStatusPending:
			stats.Pending = c.N
		case models.JobStatusRunning:
			stats.Running = c.N
		case models.JobStatusSucceeded:
			stats.Succeeded = c.N
		case models.JobStatusDead:
			stats.Dead = c.N
		}
	}

	var due struct {
		Due    int64
		Micros sql.NullInt64
	}

	if err := db.Table("jobs").
		Select("COUNT(*) AS due, " + jobAgeSQL(dialect) + " AS micros").
		Where("status = 'pending' AND run_at <= " + jobNowSQL(dialect)).
		Scan(&due).Error; err != nil {
		return models.JobStats{}, repoError(ctx, "job stats", err)
	}

	stats.Due = due.Due
	if due.Micros.Valid && due.Micros.Int64 > 0 {
		stats.OldestDueAge = time.Duration(due.Micros.Int64) * time.Microsecond
	}

	return stats, nil
}

// DeleteFinishedBefore implements models.JobRepository. It selects the IDs
// first and deletes them by primary key, so the statement is bounded and
// portable (no DELETE ... LIMIT).
func (r *GORMJobRepository) DeleteFinishedBefore(ctx context.Context, status string, maxAge time.Duration, limit int) (int64, error) {
	if status != models.JobStatusSucceeded && status != models.JobStatusDead {
		return 0, dberrors.NewDatabaseError("job cleanup", fmt.Errorf("%w: status", dberrors.ErrValidation))
	}

	if limit < 1 || maxAge < 0 {
		return 0, validationError("job cleanup", errors.New("a positive limit and a non-negative age are required"))
	}

	dialect := r.DB.Dialector.Name()

	var deleted int64

	err := r.Write(ctx, func(db *gorm.DB) error {
		deleted = 0

		var ids []uint64
		if err := db.Table("jobs").
			Where("status = ? AND finished_at < "+jobOffsetSQL(dialect), status, jobOffsetArg(dialect, -maxAge)).
			Order("finished_at ASC").Order("id ASC").
			Limit(limit).
			Pluck("id", &ids).Error; err != nil {
			return err
		}

		if len(ids) == 0 {
			return nil
		}

		res := db.Exec("DELETE FROM jobs WHERE id IN ? AND status = ?", ids, status)
		deleted = res.RowsAffected

		return res.Error
	})

	return deleted, repoError(ctx, "job cleanup", err)
}
