package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"backend/internal/models"
)

// Job is the job that a Handler receives.
type Job struct {
	// Type is the registered type name.
	Type string
	// Payload is the JSON document that was enqueued.
	Payload json.RawMessage
	// ID is the row ID. It is stable over the attempts, so it can serve as
	// an idempotency key.
	ID uint64
	// Attempt is the number of this attempt, starting at 1. A crash counts.
	Attempt int
	// MaxAttempts is the attempt after which a failure makes the job dead.
	MaxAttempts int
}

// Handler runs one job. A nil return marks the job succeeded, an error
// schedules a retry (or the dead state after the last attempt) and
// Permanent(err) marks it dead at once. ctx ends when the job is lost (its
// lease could not be kept) or the process shuts down: a handler must stop
// then. A handler can run more than once for a job, so it must be idempotent.
type Handler func(ctx context.Context, job Job) error

// ValidType reports whether name is a valid job type: 1 to 100 characters of
// a-z, 0-9, ".", "_" and "-".
func ValidType(name string) bool {
	if name == "" || len(name) > models.JobTypeMaxLen {
		return false
	}

	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}

	return true
}

// Registry maps a job type to the Handler of this process. It is safe for
// concurrent use.
type Registry struct {
	handlers map[string]Handler
	mu       sync.RWMutex
}

// NewRegistry returns an empty registry. The template registers no type: an
// application registers its own, before the pool starts.
func NewRegistry() *Registry {
	return &Registry{handlers: map[string]Handler{}}
}

// Register adds the handler of a job type. An invalid or duplicate type name
// and a nil handler are programming errors: Register panics, so the process
// does not start.
func (r *Registry) Register(typ string, h Handler) {
	if !ValidType(typ) {
		panic(fmt.Sprintf("jobs: invalid job type %q: use 1 to %d characters of a-z, 0-9, '.', '_' and '-'", typ, models.JobTypeMaxLen))
	}

	if h == nil {
		panic(fmt.Sprintf("jobs: nil handler for job type %q", typ))
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, dup := r.handlers[typ]; dup {
		panic(fmt.Sprintf("jobs: job type %q is registered twice", typ))
	}

	r.handlers[typ] = h
}

// Lookup returns the handler of a type.
func (r *Registry) Lookup(typ string) (Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	h, ok := r.handlers[typ]

	return h, ok
}

// Types returns the registered type names, sorted.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	types := make([]string, 0, len(r.handlers))
	for typ := range r.handlers {
		types = append(types, typ)
	}

	slices.Sort(types)

	return types
}
