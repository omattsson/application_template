package jobs

import (
	"errors"
	"fmt"
)

// Errors of Queue.Enqueue. They are the caller's mistakes: a handler of the API
// maps them to a 500 (the type and the payload come from code, not from a
// request), never to a message for the client.
var (
	// ErrDisabled is returned by a nil Queue: the module is off.
	ErrDisabled = errors.New("jobs: the job module is disabled")
	// ErrInvalidType is a type name that is not valid (see ValidType).
	ErrInvalidType = errors.New("jobs: invalid job type")
	// ErrPayloadTooLarge is a payload over 64 KiB once marshalled.
	ErrPayloadTooLarge = errors.New("jobs: payload is larger than 64 KiB")
	// ErrInvalidPayload is a payload that cannot be marshalled to JSON.
	ErrInvalidPayload = errors.New("jobs: payload is not valid JSON")
	// ErrInvalidOption is an option value that is out of range.
	ErrInvalidOption = errors.New("jobs: invalid enqueue option")
)

// permanentError marks a failure that a retry cannot fix.
type permanentError struct {
	err error
}

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent wraps err so that the job becomes dead at once, without another
// attempt: return it for a failure that a retry cannot fix (a payload that
// does not make sense, a record that is gone). Permanent(nil) is nil, so a
// handler can return it for the result of a call.
func Permanent(err error) error {
	if err == nil {
		return nil
	}

	return &permanentError{err: err}
}

// IsPermanent reports whether err was wrapped with Permanent.
func IsPermanent(err error) bool {
	var p *permanentError

	return errors.As(err, &p)
}

func invalidOption(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidOption, fmt.Sprintf(format, args...))
}
