package ctxutil

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type ctxKey struct{}

// blockingWrite stands for a database call that waits for a pool connection
// or a lock until its context ends.
func blockingWrite(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

type parentCase struct {
	name string
	// parent returns the parent context and a function that ends it
	// (nil: the parent does not end).
	parent func() (context.Context, func())
}

func parentCases() []parentCase {
	return []parentCase{
		{
			name: "the client leaves",
			parent: func() (context.Context, func()) {
				ctx, cancel := context.WithCancel(context.Background())
				return ctx, cancel
			},
		},
		{
			name: "the parent has a shorter deadline",
			parent: func() (context.Context, func()) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				return ctx, cancel
			},
		},
		{
			name: "the parent stays open",
			parent: func() (context.Context, func()) {
				return context.Background(), nil
			},
		},
	}
}

func TestDetachSurvivesParentCancel(t *testing.T) {
	t.Parallel()

	for _, tt := range parentCases() {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent, end := tt.parent()

			const limit = 200 * time.Millisecond
			start := time.Now()
			ctx, cancel := Detach(parent, limit)
			defer cancel()
			if end != nil {
				end()
				<-parent.Done()
			}

			assert.NoError(t, ctx.Err(), "the end of the parent does not end the detached context")

			err := blockingWrite(ctx)
			assert.True(t, errors.Is(err, context.DeadlineExceeded), "a blocked write ends with DeadlineExceeded, got %v", err)
			assert.GreaterOrEqual(t, time.Since(start), limit, "the write ran until the limit, not until the parent ended")
		})
	}
}

func TestDetachKeepsValues(t *testing.T) {
	t.Parallel()

	for _, tt := range parentCases() {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent, end := tt.parent()
			parent = context.WithValue(parent, ctxKey{}, "request-1")

			ctx, cancel := Detach(parent, time.Minute)
			defer cancel()
			if end != nil {
				end()
			}

			assert.Equal(t, "request-1", ctx.Value(ctxKey{}), "the values of the parent stay")
		})
	}
}

func TestDetachHasDeadline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		timeout time.Duration
	}{
		{name: "short timeout", timeout: 50 * time.Millisecond},
		{name: "write timeout", timeout: WriteTimeout},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			before := time.Now()
			ctx, cancel := Detach(context.Background(), tt.timeout)
			defer cancel()
			after := time.Now()

			deadline, ok := ctx.Deadline()
			require.True(t, ok, "a detached context has a deadline")
			assert.False(t, deadline.Before(before.Add(tt.timeout)), "the deadline is not earlier than timeout")
			assert.False(t, deadline.After(after.Add(tt.timeout)), "the deadline is within timeout")
		})
	}
}

func TestDetachCancelEndsTheContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := Detach(context.Background(), WriteTimeout)
	cancel()
	assert.ErrorIs(t, ctx.Err(), context.Canceled)
}

// TestNoUnboundedDetach keeps one rule for the whole backend: code detaches
// from a context only through Detach, which always sets a deadline. A bare
// context.WithoutCancel has none, so a stalled database could hold the work
// forever. It is a text match: an aliased context import, or a write on
// context.Background() without WithTimeout, is not caught; review those.
func TestNoUnboundedDetach(t *testing.T) {
	t.Parallel()

	// The test runs in backend/internal/ctxutil; the walk covers backend/.
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "vendor" || name == "tmp" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Dir(path) == filepath.Join(root, "internal", "ctxutil") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checked++
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(src), "\n") {
			code, _, _ := strings.Cut(line, "//")
			assert.NotContains(t, code, "context.WithoutCancel(",
				"%s:%d detaches without a deadline; use ctxutil.Detach", rel, i+1)
		}
		return nil
	})
	require.NoError(t, err)
	assert.Greater(t, checked, 10, "the walk found the backend sources")
}
