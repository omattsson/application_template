package mail

import (
	"context"
	"sync"
	"time"
)

// FakeMailer is a Mailer for tests: it records the messages it is given and
// can be made to fail or to take its time. The zero value is ready to use.
type FakeMailer struct {
	err   error
	sent  []Message
	calls int
	delay time.Duration
	mu    sync.Mutex
}

var _ Mailer = (*FakeMailer)(nil)

// Send validates msg as the real mailer does, waits for the delay (or ctx),
// then fails with the configured error or records the message.
func (f *FakeMailer) Send(ctx context.Context, msg Message) error {
	f.mu.Lock()
	f.calls++
	delay, err := f.delay, f.err
	f.mu.Unlock()

	if verr := msg.Validate(); verr != nil {
		return verr
	}

	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if err != nil {
		return err
	}

	f.mu.Lock()
	f.sent = append(f.sent, msg)
	f.mu.Unlock()

	return nil
}

// SetError makes every later Send fail with err (nil: succeed again).
func (f *FakeMailer) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.err = err
}

// SetDelay makes every later Send wait for d before it answers.
func (f *FakeMailer) SetDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.delay = d
}

// Messages returns a copy of the messages that were sent successfully.
func (f *FakeMailer) Messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]Message(nil), f.sent...)
}

// Calls returns the number of Send calls, failed ones included.
func (f *FakeMailer) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

// Reset forgets the recorded messages and the call count (not the error or
// the delay).
func (f *FakeMailer) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sent, f.calls = nil, 0
}
