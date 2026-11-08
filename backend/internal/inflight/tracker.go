// Package inflight tracks background work that outlives the request (or the
// worker run) that started it, so that shutdown can wait for it before the
// database closes.
package inflight

import (
	"context"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Tracker runs and counts in-flight work. Start work with Go; shutdown calls
// Wait once. The zero value is ready to use. A Tracker must not be copied.
//
// sync.WaitGroup requires that an Add which raises the counter from zero
// happens before Wait. Tracker keeps that rule: Wait first marks the tracker
// closed under the same lock that Go holds for Add, so once shutdown began no
// new work is accepted and Go returns false.
type Tracker struct {
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

// New returns an empty Tracker.
func New() *Tracker {
	return &Tracker{}
}

// Go runs fn in a new goroutine that Wait waits for, and returns true. After
// Wait was called (shutdown began) it does not run fn and returns false: the
// caller then does the work inline, or drops it. A nil Tracker accepts no
// work either. A panic in fn is recovered and logged with the function:line
// of the top frames of the panic site; the job then counts as done.
func (t *Tracker) Go(fn func()) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return false
	}
	t.wg.Add(1)
	t.mu.Unlock()

	go func() {
		defer t.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("In-flight work panicked", "panic", r, "panic_site", panicSite(panicSiteFrames))
			}
		}()
		fn()
	}()
	return true
}

// panicSiteFrames is the number of frames of the panic site that a recovered
// panic logs.
const panicSiteFrames = 3

// panicSite returns up to n frames of the code that panicked, as
// "function:line", innermost first. Call it from the deferred function that
// recovered: the frames up to runtime.gopanic and the runtime frames after
// it (a nil dereference, a map write) are skipped.
func panicSite(n int) []string {
	if n <= 0 {
		return nil
	}
	pcs := make([]uintptr, 32)
	pcs = pcs[:runtime.Callers(2, pcs)]
	frames := runtime.CallersFrames(pcs)

	sawPanic := false
	site := make([]string, 0, n)
	for len(site) < n {
		frame, more := frames.Next()
		switch {
		case frame.Function == "runtime.gopanic":
			sawPanic = true
		case sawPanic && !strings.HasPrefix(frame.Function, "runtime."):
			site = append(site, frame.Function+":"+strconv.Itoa(frame.Line))
		}
		if !more {
			break
		}
	}
	return site
}

// Wait stops accepting new work and waits until the started work finished or
// ctx ends. It returns ctx.Err() when ctx ended first; the work then keeps
// running (the process is exiting). A nil Tracker has no work: Wait returns
// nil.
func (t *Tracker) Wait(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()

	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// Work that finished at the deadline is not a timeout.
		select {
		case <-done:
			return nil
		default:
			return ctx.Err()
		}
	}
}
