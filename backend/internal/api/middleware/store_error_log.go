package middleware

import (
	"context"
	"log/slog"
	"time"

	"backend/internal/logutil"
)

// storeErrorLogInterval is the time between two Error lines of one
// storeErrorLog.
const storeErrorLogInterval = time.Minute

// storeErrorLog logs the failed lookups of an auth middleware without
// flooding the log: when the session store or the database is down, every
// authenticated request fails the same lookup, and one Error line per request
// would drown the log (and the one line that says why). It writes one Error
// line per interval, with the number of lines it left out, and the others at
// Debug level. The outcome metrics (auth.jwt.total, auth.apikey.total) count
// every failure.
type storeErrorLog struct {
	limiter *logutil.Limiter
}

func newStoreErrorLog() *storeErrorLog {
	return &storeErrorLog{limiter: logutil.NewLimiter(storeErrorLogInterval)}
}

// newStoreErrorLogWithClock is newStoreErrorLog with its own clock (tests).
func newStoreErrorLogWithClock(now func() time.Time) *storeErrorLog {
	return &storeErrorLog{limiter: logutil.NewLimiterWithClock(storeErrorLogInterval, now)}
}

// log writes msg with attrs.
func (l *storeErrorLog) log(ctx context.Context, msg string, attrs ...any) {
	if l == nil {
		slog.ErrorContext(ctx, msg, attrs...)
		return
	}
	if ok, suppressed := l.limiter.Allow(); ok {
		if suppressed > 0 {
			attrs = append(attrs, "suppressed", suppressed)
		}
		slog.ErrorContext(ctx, msg, attrs...)
		return
	}
	slog.DebugContext(ctx, msg, attrs...)
}
