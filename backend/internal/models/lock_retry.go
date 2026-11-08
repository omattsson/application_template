package models

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/go-sql-driver/mysql"
)

// MySQL error numbers that InnoDB answers after it rolled back the statement
// or the transaction, so that running it again is safe.
const (
	mysqlErrLockWaitTimeout = 1205
	mysqlErrDeadlock        = 1213
)

const (
	// lockRetryMax is the number of retries after the first attempt.
	lockRetryMax     = 3
	lockRetryMinWait = 10 * time.Millisecond
	lockRetryMaxWait = 50 * time.Millisecond
)

// isLockError reports whether err is a MySQL deadlock (1213) or lock wait
// timeout (1205), also when wrapped. Nothing else is retried.
func isLockError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) {
		return false
	}
	return mysqlErr.Number == mysqlErrDeadlock || mysqlErr.Number == mysqlErrLockWaitTimeout
}

// RetryOnLock runs fn and runs it again after a deadlock or lock wait
// timeout, up to lockRetryMax more times with a short backoff. It returns the
// error of the last attempt. Any other error is returned at once.
//
// fn must be one complete statement or one whole transaction: the server
// rolled back the failed attempt, so a retry applies the write once. Never
// call it around a single statement inside a larger transaction (InnoDB
// rolled back the whole transaction on a deadlock); wrap the transaction
// instead, as database.BaseRepository.Tx does. fn must reset any in-memory
// state it changed before it returns an error.
//
// When ctx is done, RetryOnLock stops and the error matches the context error
// as well as the lock error, so a cancelled request still maps to 499.
func RetryOnLock(ctx context.Context, fn func() error) error {
	return retryOnLockWait(ctx, lockRetryWait, fn)
}

func retryOnLockWait(ctx context.Context, wait func(attempt int) time.Duration, fn func() error) error {
	var err error
	for attempt := 0; attempt <= lockRetryMax; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if !isLockError(err) {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: %w", ctxErr, err)
		}
		if attempt == lockRetryMax {
			break
		}
		if !sleepOrDone(ctx, wait(attempt+1)) {
			return fmt.Errorf("%w: %w", ctx.Err(), err)
		}
	}
	slog.Warn("Database write failed on a lock after all retries", "retries", lockRetryMax, "error", err)
	return err
}

// lockRetryWait returns the backoff before retry number attempt (1-based):
// a jittered wait that grows with each attempt.
func lockRetryWait(attempt int) time.Duration {
	// #nosec G404 -- retry jitter, not a secret.
	jitter := lockRetryMinWait + rand.N(lockRetryMaxWait-lockRetryMinWait+1)
	return jitter * time.Duration(attempt)
}

// sleepOrDone waits for d and reports false when ctx ends first.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
