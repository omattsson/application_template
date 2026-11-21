package jobs

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func noop(context.Context, Job) error { return nil }

func TestValidType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		typ  string
		want bool
	}{
		{"simple", "send-mail", true},
		{"all allowed characters", "a.b_c-d9", true},
		{"one character", "a", true},
		{"100 characters", strings.Repeat("a", 100), true},
		{"101 characters", strings.Repeat("a", 101), false},
		{"empty", "", false},
		{"upper case", "Mail", false},
		{"space", "send mail", false},
		{"slash", "a/b", false},
		{"colon", "a:b", false},
		{"unicode", "mäil", false},
		{"newline", "a\nb", false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ValidType(tt.typ))
		})
	}
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	assert.Empty(t, r.Types())

	r.Register("b", noop)
	r.Register("a", noop)

	assert.Equal(t, []string{"a", "b"}, r.Types(), "sorted")
	h, ok := r.Lookup("a")
	assert.True(t, ok)
	assert.NotNil(t, h)
	_, ok = r.Lookup("c")
	assert.False(t, ok)
}

func TestRegistryRegisterPanics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		fn   func(r *Registry)
		name string
		want string
	}{
		{func(r *Registry) { r.Register("a", noop); r.Register("a", noop) }, "duplicate", "registered twice"},
		{func(r *Registry) { r.Register("", noop) }, "empty type", "invalid job type"},
		{func(r *Registry) { r.Register("Bad Name", noop) }, "invalid type", "invalid job type"},
		{func(r *Registry) { r.Register("a", nil) }, "nil handler", "nil handler"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := NewRegistry()
			defer func() {
				msg, _ := recover().(string)
				assert.Contains(t, msg, tt.want)
			}()
			tt.fn(r)
			t.Fatal("Register did not panic")
		})
	}
}
