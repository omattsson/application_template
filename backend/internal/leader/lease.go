package leader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"backend/internal/ctxutil"
	"backend/pkg/dberrors"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Defaults of LeaseConfig.
const (
	DefaultLeaseDuration      = 15 * time.Second
	DefaultLeaseRenewDeadline = 10 * time.Second
	DefaultLeaseRetryPeriod   = 2 * time.Second
)

// releaseTimeout bounds the release of the lease when Run ends. The release
// runs on a context detached from the cancelled one, so it also works during
// shutdown.
const releaseTimeout = 2 * time.Second

// LeaseConfig is the timing of a Lease.
//
//   - Duration: how long a lease is valid after the last successful
//     acquisition or renewal (database time). Another replica can take the
//     lease only after it expired.
//   - RenewDeadline: how long the holder keeps acting as leader without a
//     successful renewal (measured on its own monotonic clock from the start
//     of its last successful renewal). It must be shorter than Duration: the
//     difference is the time the holder's leader work has to stop before
//     another replica can start.
//   - RetryPeriod: the interval between two acquisition or renewal attempts.
//     It must be shorter than RenewDeadline, so a holder tries more than once
//     before it gives up.
type LeaseConfig struct {
	Duration      time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// DefaultLeaseConfig returns 15 s, 10 s and 2 s: a crashed leader is
// replaced within about 17 s, and a leader that cannot reach the database
// stops its work 5 s before its lease expires.
func DefaultLeaseConfig() LeaseConfig {
	return LeaseConfig{
		Duration:      DefaultLeaseDuration,
		RenewDeadline: DefaultLeaseRenewDeadline,
		RetryPeriod:   DefaultLeaseRetryPeriod,
	}
}

// Validate checks that every value is positive and at least a millisecond
// (the precision of the expiry column) and that RetryPeriod <
// RenewDeadline < Duration.
func (c LeaseConfig) Validate() error {
	if c.Duration <= 0 || c.RenewDeadline <= 0 || c.RetryPeriod <= 0 {
		return errors.New("lease Duration, RenewDeadline and RetryPeriod must be positive")
	}
	if c.Duration < time.Millisecond || c.RenewDeadline < time.Millisecond || c.RetryPeriod < time.Millisecond {
		return errors.New("lease Duration, RenewDeadline and RetryPeriod must be at least a millisecond")
	}
	if c.RenewDeadline >= c.Duration {
		return fmt.Errorf("lease RenewDeadline (%s) must be shorter than Duration (%s)", c.RenewDeadline, c.Duration)
	}
	if c.RetryPeriod >= c.RenewDeadline {
		return fmt.Errorf("lease RetryPeriod (%s) must be shorter than RenewDeadline (%s)", c.RetryPeriod, c.RenewDeadline)
	}
	return nil
}

// LeaseRecord is a row of leader_leases: one row per lease name. Its
// definition is copied into migration 20260101000003 (leaderLeaseV1); a
// change here needs a new migration, and TestReplicaTablesMatchModels in the
// database package fails without one.
//
// ExpiresAt is written and compared only in SQL with the database clock
// (UTC_TIMESTAMP(3) on MySQL); the lease never sends or reads a Go time, so
// the driver's time zone and parseTime settings do not matter.
type LeaseRecord struct {
	ExpiresAt time.Time `gorm:"column:expires_at;type:datetime(3);not null"`
	Name      string    `gorm:"column:name;primaryKey;size:64"`
	Holder    string    `gorm:"column:holder;size:253;not null"`
}

// TableName is the name of the table.
func (LeaseRecord) TableName() string { return "leader_leases" }

// Lease is a leader election on a row of leader_leases. Every replica runs
// a Lease with the same name and its own holder ID; at most one of them
// holds the lease at a time.
//
// Time source: the expiry is set and compared with the database server
// clock in the same statement, so the clocks of the replicas never decide
// who may take the lease. The holder measures its own RenewDeadline with its
// monotonic clock from the moment it SENT its last successful renewal; the
// database evaluated that renewal later, so the holder's deadline always
// comes RenewDeadline-before-Duration ahead of the expiry the database
// stored. All replicas must use the same MySQL server (the primary).
//
// Fencing: the lease stops a holder that cannot renew, but it cannot stop a
// process that is paused (a long GC or a frozen VM) from finishing a write
// it had started before the pause. Leader work must stop within Duration -
// RenewDeadline after its context is cancelled and should be idempotent.
type Lease struct {
	db     *gorm.DB
	sql    leaseSQL
	name   string
	holder string
	cfg    LeaseConfig
}

// NewLease returns the lease name for the replica holder on db (the table
// leader_leases must exist, migration 20260101000003). Zero fields of cfg
// take the defaults (DefaultLeaseConfig). It panics on an invalid
// configuration, an empty name or holder or a nil db: these are programming
// errors, the values do not come from user input.
func NewLease(db *gorm.DB, name, holder string, cfg LeaseConfig) *Lease {
	def := DefaultLeaseConfig()
	if cfg.Duration == 0 {
		cfg.Duration = def.Duration
	}
	if cfg.RenewDeadline == 0 {
		cfg.RenewDeadline = def.RenewDeadline
	}
	if cfg.RetryPeriod == 0 {
		cfg.RetryPeriod = def.RetryPeriod
	}
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	if db == nil || name == "" || holder == "" {
		panic("leader.NewLease needs a database, a name and a holder")
	}
	// The lease statements run on a session with a silent GORM logger: during
	// an outage one statement fails every RetryPeriod, and the logger would
	// write an Error line for each. Run logs the outage once itself.
	if db.Logger != nil {
		db = db.Session(&gorm.Session{Logger: db.Logger.LogMode(logger.Silent)})
	}
	return &Lease{db: db, sql: sqlFor(db.Dialector.Name()), name: name, holder: holder, cfg: cfg}
}

// Holder returns the holder ID of this replica.
func (l *Lease) Holder() string { return l.holder }

// Run campaigns for the lease until ctx is cancelled and blocks until then.
//
// When this replica acquires the lease, Run calls onStarted in a goroutine
// of its own with a context that is cancelled when the leadership ends, and
// renews every RetryPeriod. Leadership ends when
//   - a renewal shows another holder (deposed): at once;
//   - no renewal succeeded for RenewDeadline (database errors, timeouts):
//     before the lease can expire, so no other replica can have started;
//   - ctx is cancelled.
//
// Then Run cancels the onStarted context, waits until onStarted returned,
// calls onStopped (exactly once per term, never without onStarted) and,
// unless ctx was cancelled, campaigns again. onStarted must return once its
// context is cancelled (leader.Group.Start returns at once).
//
// When ctx is cancelled Run releases the lease (expiry set in the past, only
// where this replica is the holder) with a short detached context, so the
// next replica takes over at its next attempt instead of after Duration.
func (l *Lease) Run(ctx context.Context, onStarted func(ctx context.Context), onStopped func()) {
	defer l.releaseOnExit(ctx)
	var c campaigner
	for {
		sent, ok := l.campaign(ctx, &c)
		if !ok {
			return
		}
		l.lead(ctx, sent, onStarted, onStopped)
		if ctx.Err() != nil {
			return
		}
	}
}

// campaigner tracks a campaign error, so a failing database is logged once
// until it works again.
type campaigner struct {
	failing bool
}

// campaign tries to acquire the lease every RetryPeriod until it holds it
// or ctx ends. It returns the monotonic time at which the successful
// attempt was sent, and false when ctx ended.
func (l *Lease) campaign(ctx context.Context, c *campaigner) (time.Time, bool) {
	for {
		sent := time.Now()
		callCtx, cancel := context.WithTimeout(ctx, l.cfg.RenewDeadline)
		ok, holder, err := l.tryAcquire(callCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return time.Time{}, false
			}
			if !c.failing {
				slog.Warn("Leader lease: campaign failed; retrying", "lease", l.name, "holder", l.holder, "error", err)
				c.failing = true
			}
		} else {
			if c.failing {
				slog.Info("Leader lease: database reachable again", "lease", l.name, "holder", l.holder)
				c.failing = false
			}
			switch {
			case !ok:
				slog.Debug("Leader lease held by another replica", "lease", l.name, "holder", holder)
			case time.Since(sent) < l.cfg.RenewDeadline-l.cfg.RetryPeriod:
				return sent, true
			default:
				// Acquired, but so late that the first renewal (one
				// RetryPeriod from now) would be due after the renew
				// deadline: the term would stop at once. The next attempt
				// renews it (the row names this holder).
				slog.Warn("Leader lease: acquisition answered too late to lead; retrying", "lease", l.name, "holder", l.holder)
			}
		}
		if !sleepCtx(ctx, l.cfg.RetryPeriod) {
			return time.Time{}, false
		}
	}
}

// lead runs one leadership term. sent is when the acquisition was sent.
func (l *Lease) lead(ctx context.Context, sent time.Time, onStarted func(context.Context), onStopped func()) {
	slog.Info("Leader lease acquired; this replica is the leader", "lease", l.name, "holder", l.holder)
	termCtx, cancel := context.WithCancel(ctx)
	started := make(chan struct{})
	go func() {
		defer close(started)
		onStarted(termCtx)
	}()
	defer func() {
		cancel()
		<-started
		onStopped()
	}()

	// deadline is when this replica stops acting as leader without a
	// successful renewal. time.Now carries the monotonic clock, so a wall
	// clock step does not move it.
	deadline := sent.Add(l.cfg.RenewDeadline)
	timer := time.NewTimer(l.cfg.RetryPeriod)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("Leader lease: stopping (shutdown)", "lease", l.name, "holder", l.holder)
			return
		case <-timer.C:
		}
		if !time.Now().Before(deadline) {
			slog.Error("Leader lease: no successful renewal within the renew deadline; stopping leader work", "lease", l.name, "holder", l.holder, "renew_deadline", l.cfg.RenewDeadline)
			return
		}
		attempt := time.Now()
		// The call may not run past the deadline: a database that does not
		// answer must not keep this replica leader.
		callCtx, cancelCall := context.WithDeadline(ctx, deadline)
		ok, holder, err := l.tryAcquire(callCtx)
		cancelCall()
		switch {
		case err == nil && ok:
			deadline = attempt.Add(l.cfg.RenewDeadline)
			timer.Reset(l.cfg.RetryPeriod)
			continue
		case err == nil:
			slog.Warn("Leader lease: another replica holds the lease; stopping leader work", "lease", l.name, "holder", l.holder, "new_holder", holder)
			return
		case ctx.Err() != nil:
			slog.Info("Leader lease: stopping (shutdown)", "lease", l.name, "holder", l.holder)
			return
		}
		left := time.Until(deadline)
		if left <= 0 {
			slog.Error("Leader lease: renewal failed and the renew deadline passed; stopping leader work", "lease", l.name, "holder", l.holder, "error", err)
			return
		}
		slog.Warn("Leader lease: renewal failed; retrying", "lease", l.name, "holder", l.holder, "error", err, "left", left)
		timer.Reset(min(l.cfg.RetryPeriod, left))
	}
}

// releaseOnExit releases the lease after Run ended. It runs also when this
// replica is not the holder (the statement then matches nothing), which
// covers an acquisition that succeeded in the database but whose answer was
// lost to the cancellation.
func (l *Lease) releaseOnExit(ctx context.Context) {
	rctx, cancel := ctxutil.Detach(ctx, releaseTimeout)
	defer cancel()
	if err := l.release(rctx); err != nil {
		slog.Warn("Leader lease: release failed; the lease expires on its own", "lease", l.name, "holder", l.holder, "error", err, "within", l.cfg.Duration)
	}
}

// tryAcquire acquires or renews the lease in one atomic UPDATE (the row is
// free when it expired by the database clock or already names this
// holder), and inserts the row when it does not exist. It returns whether
// this replica holds the lease and the holder in the row.
//
// MySQL reports changed rows, not matched rows: an UPDATE that sets the
// same values (a renewal within the same millisecond) reports 0. So after 0
// rows the holder is read back: only this replica ever writes its own ID
// into the row, so a row that names it was matched by the UPDATE.
func (l *Lease) tryAcquire(ctx context.Context) (bool, string, error) {
	dur := l.sql.offset(l.cfg.Duration)
	res := l.db.WithContext(ctx).Exec(l.sql.acquire, l.holder, dur, l.name, l.holder)
	if res.Error != nil {
		return false, "", res.Error
	}
	if res.RowsAffected > 0 {
		return true, l.holder, nil
	}
	var holders []string
	if err := l.db.WithContext(ctx).Raw(l.sql.holder, l.name).Scan(&holders).Error; err != nil {
		return false, "", err
	}
	if len(holders) > 0 {
		return holders[0] == l.holder, holders[0], nil
	}
	ok, err := l.insert(ctx)
	if err != nil {
		return false, "", err
	}
	if ok {
		return true, l.holder, nil
	}
	return false, "", nil
}

// insert creates the lease row for this holder. A duplicate key means that
// another replica inserted it first: not acquired, not an error.
func (l *Lease) insert(ctx context.Context) (bool, error) {
	err := l.db.WithContext(ctx).Exec(l.sql.insert, l.name, l.holder, l.sql.offset(l.cfg.Duration)).Error
	if err == nil {
		return true, nil
	}
	if dberrors.IsDuplicateKey(err) {
		return false, nil
	}
	return false, err
}

// release sets the expiry in the past where this replica is the holder.
func (l *Lease) release(ctx context.Context) error {
	return l.db.WithContext(ctx).Exec(l.sql.release, l.name, l.holder).Error
}

// leaseSQL holds the statements of one dialect. They are constants; the
// values are bound as arguments.
type leaseSQL struct {
	// offset turns a duration into the argument of the "now plus" term.
	offset  func(time.Duration) any
	acquire string // args: holder, offset(Duration), name, holder
	holder  string // args: name
	insert  string // args: name, holder, offset(Duration)
	release string // args: name, holder
}

// sqlFor returns the statements of the dialect: MySQL (production), SQLite
// otherwise (unit tests). MySQL's UTC_TIMESTAMP(3) does not depend on the
// session time zone and is constant within one statement.
func sqlFor(dialect string) leaseSQL {
	if dialect == "mysql" {
		const now = "UTC_TIMESTAMP(3)"
		return leaseSQL{
			offset:  func(d time.Duration) any { return d.Microseconds() },
			acquire: "UPDATE leader_leases SET holder = ?, expires_at = " + now + " + INTERVAL ? MICROSECOND WHERE name = ? AND (holder = ? OR expires_at < " + now + ")",
			holder:  "SELECT holder FROM leader_leases WHERE name = ?",
			insert:  "INSERT INTO leader_leases (name, holder, expires_at) VALUES (?, ?, " + now + " + INTERVAL ? MICROSECOND)",
			release: "UPDATE leader_leases SET expires_at = " + now + " - INTERVAL 1 SECOND WHERE name = ? AND holder = ?",
		}
	}
	const now = "strftime('%Y-%m-%d %H:%M:%f', 'now')"
	return leaseSQL{
		offset:  func(d time.Duration) any { return fmt.Sprintf("%+.3f seconds", d.Seconds()) },
		acquire: "UPDATE leader_leases SET holder = ?, expires_at = strftime('%Y-%m-%d %H:%M:%f', 'now', ?) WHERE name = ? AND (holder = ? OR expires_at < " + now + ")",
		holder:  "SELECT holder FROM leader_leases WHERE name = ?",
		insert:  "INSERT INTO leader_leases (name, holder, expires_at) VALUES (?, ?, strftime('%Y-%m-%d %H:%M:%f', 'now', ?))",
		release: "UPDATE leader_leases SET expires_at = strftime('%Y-%m-%d %H:%M:%f', 'now', '-1 seconds') WHERE name = ? AND holder = ?",
	}
}

// sleepCtx waits d or until ctx ends; it returns false when ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
