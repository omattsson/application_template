package mail

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"backend/internal/ctxutil"
	"backend/internal/logutil"
)

// Health check settings.
const (
	// HealthCacheTTL is how long the result of a probe is reused: the readiness
	// endpoint is public and polled often, and each probe opens a connection to
	// the mail server.
	HealthCacheTTL = 30 * time.Second
	// HealthProbeTimeout bounds one probe.
	HealthProbeTimeout = 5 * time.Second
)

// ErrUnavailable is the only error that a health check reports. The readiness
// endpoint is public: the real cause (host, port, certificate, server answer)
// goes to the log, never into the answer.
var ErrUnavailable = errors.New("mail server unavailable")

// NewHealthCheck returns a check for healthChecker.AddInformationalCheck. It calls ping
// (connect, EHLO, STARTTLS, authentication, quit; no message), at most once per
// ttl: the result, error included, is cached, and concurrent callers share one
// probe. A probe has its own deadline of timeout on a context that the caller
// cannot cancel, so a readiness request that ends early does not store its
// cancellation as the result. now may be nil (time.Now); tests set it.
func NewHealthCheck(ping func(context.Context) error, ttl, timeout time.Duration, now func() time.Time) func(context.Context) error {
	if now == nil {
		now = time.Now
	}

	var (
		mu      sync.Mutex
		at      time.Time
		last    error
		warned  logutil.Limiter
		checked bool
	)

	return func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()

		if checked && now().Sub(at) < ttl {
			return last
		}

		pctx, cancel := ctxutil.Detach(ctx, timeout)
		defer cancel()

		err := ping(pctx)

		checked, at = true, now()
		if err == nil {
			last = nil

			return nil
		}

		last = ErrUnavailable

		if ok, suppressed := warned.Allow(); ok {
			slog.Warn("Mail server health check failed", "error", err, "suppressed", suppressed)
		} else {
			slog.Debug("Mail server health check failed", "error", err)
		}

		return last
	}
}
