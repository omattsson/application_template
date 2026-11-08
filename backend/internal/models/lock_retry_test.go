package models

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"backend/pkg/dberrors"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errTestDeadlock    = &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock; try restarting transaction"}
	errTestLockTimeout = &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded; try restarting transaction"}
	errTestDuplicate   = &mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'x' for key 'name'"}
)

// noWait keeps the retry tests fast.
func noWait(int) time.Duration { return 0 }

func TestRetryOnLockRetriesDeadlockOnce(t *testing.T) {
	t.Parallel()

	calls, applied := 0, 0
	err := retryOnLockWait(context.Background(), noWait, func() error {
		calls++
		if calls == 1 {
			// The first attempt was rolled back by the server: no side effect.
			return errTestDeadlock
		}
		applied++
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 2, calls, "one failure and one retry")
	assert.Equal(t, 1, applied, "the write is applied once")
}

func TestRetryOnLock(t *testing.T) {
	t.Parallel()

	errOther := errors.New("driver: bad connection")
	wrapped := dberrors.NewDatabaseError("update", fmt.Errorf("wrap: %w", errTestDeadlock))

	tests := []struct {
		name      string
		results   []error // result of each attempt; success after the end
		wantErr   error
		wantCalls int
	}{
		{name: "success at once", results: []error{nil}, wantCalls: 1},
		{name: "lock wait timeout, then success", results: []error{errTestLockTimeout, nil}, wantCalls: 2},
		{name: "wrapped deadlock, then success", results: []error{wrapped, nil}, wantCalls: 2},
		{name: "three retries, then success", results: []error{errTestDeadlock, errTestLockTimeout, errTestDeadlock, nil}, wantCalls: 4},
		{name: "gives up after 3 retries with the last error", results: []error{errTestDeadlock, errTestDeadlock, errTestDeadlock, errTestLockTimeout, nil}, wantErr: errTestLockTimeout, wantCalls: 4},
		{name: "duplicate key is not retried", results: []error{errTestDuplicate, nil}, wantErr: errTestDuplicate, wantCalls: 1},
		{name: "other error is not retried", results: []error{errOther, nil}, wantErr: errOther, wantCalls: 1},
		{name: "version mismatch is not retried", results: []error{dberrors.NewDatabaseError("update", errors.New("version mismatch")), nil}, wantErr: errors.New("version mismatch"), wantCalls: 1},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			err := retryOnLockWait(context.Background(), noWait, func() error {
				calls++
				if calls > len(tt.results) {
					return nil
				}
				return tt.results[calls-1]
			})
			if tt.wantErr == nil {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr.Error())
			}
			assert.Equal(t, tt.wantCalls, calls)
		})
	}
}

func TestRetryOnLockStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	t.Run("cancelled during the attempt", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := retryOnLockWait(ctx, noWait, func() error {
			calls++
			cancel()
			return errTestDeadlock
		})
		assert.Equal(t, 1, calls, "no retry after the cancel")
		assert.ErrorIs(t, err, context.Canceled)
		var mysqlErr *mysql.MySQLError
		assert.ErrorAs(t, err, &mysqlErr, "the lock error stays in the chain")
	})

	t.Run("cancelled during the backoff", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		start := time.Now()
		err := retryOnLockWait(ctx, func(int) time.Duration {
			go func() { time.Sleep(10 * time.Millisecond); cancel() }()
			return time.Minute
		}, func() error {
			calls++
			return errTestDeadlock
		})
		assert.Equal(t, 1, calls)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(start), 10*time.Second, "the backoff wait ends with the context")
	})

	t.Run("already cancelled but the attempt succeeds", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		err := RetryOnLock(ctx, func() error { calls++; return nil })
		assert.NoError(t, err, "a successful attempt is not turned into an error")
		assert.Equal(t, 1, calls)
	})
}

func TestRetryOnLockDefaultBackoff(t *testing.T) {
	t.Parallel()

	calls := 0
	err := RetryOnLock(context.Background(), func() error {
		calls++
		if calls < 3 {
			return errTestDeadlock
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 3, calls)

	for attempt := 1; attempt <= lockRetryMax; attempt++ {
		d := lockRetryWait(attempt)
		assert.GreaterOrEqual(t, d, lockRetryMinWait)
		assert.LessOrEqual(t, d, lockRetryMaxWait*time.Duration(attempt))
	}
}

func TestIsLockError(t *testing.T) {
	t.Parallel()

	assert.True(t, isLockError(errTestDeadlock))
	assert.True(t, isLockError(errTestLockTimeout))
	assert.True(t, isLockError(fmt.Errorf("x: %w", errTestDeadlock)))
	assert.False(t, isLockError(errTestDuplicate))
	assert.False(t, isLockError(errors.New("Error 1213 in text only")))
	assert.False(t, isLockError(nil))
}
