package sessionstore

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// defaultCleanupEvery is how often a store removes its expired entries.
const defaultCleanupEvery = time.Minute

// cleanupTimeout limits one run of the cleanup of the database store.
const cleanupTimeout = 30 * time.Second

// janitor runs a store's Cleanup on a timer in one goroutine.
type janitor struct {
	cancel context.CancelFunc
	// exited closes when the goroutine has returned.
	exited chan struct{}
	once   sync.Once
}

// startJanitor starts the goroutine. It calls run every interval until stop.
// A failed run is logged and the goroutine goes on.
func startJanitor(interval time.Duration, run func(ctx context.Context) error) *janitor {
	ctx, cancel := context.WithCancel(context.Background())
	j := &janitor{cancel: cancel, exited: make(chan struct{})}
	go func() {
		defer close(j.exited)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runCtx, cancelRun := context.WithTimeout(ctx, cleanupTimeout)
				if err := run(runCtx); err != nil && ctx.Err() == nil {
					slog.Warn("Session store cleanup failed", "error", err)
				}
				cancelRun()
			}
		}
	}()
	return j
}

// stop ends the goroutine, also a run in progress, and waits until it has
// returned. It is safe to call more than once and from several goroutines.
func (j *janitor) stop() {
	j.once.Do(j.cancel)
	<-j.exited
}
