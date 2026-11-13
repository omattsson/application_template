package dberrors

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDatabaseError(t *testing.T) {
	t.Parallel()

	t.Run("message carries the operation", func(t *testing.T) {
		t.Parallel()
		err := &DatabaseError{Op: "create item", Err: ErrNotFound}
		assert.Equal(t, "create item: record not found", err.Error())
	})

	t.Run("message without an operation is the wrapped message", func(t *testing.T) {
		t.Parallel()
		err := &DatabaseError{Err: ErrNotFound}
		assert.Equal(t, "record not found", err.Error())
	})

	t.Run("unwraps to the cause", func(t *testing.T) {
		t.Parallel()
		err := NewDatabaseError("find", ErrConnectionFailed)
		assert.ErrorIs(t, err, ErrConnectionFailed)
		var dbErr *DatabaseError
		require.ErrorAs(t, err, &dbErr)
		assert.Equal(t, "find", dbErr.Op)
	})

	t.Run("a nil error stays nil", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, NewDatabaseError("find", nil))
	})
}

func TestHandleGormError(t *testing.T) {
	t.Parallel()

	other := errors.New("bad connection")
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "record not found", err: errors.New("record not found"), want: ErrNotFound},
		{name: "duplicate key", err: errors.New("UNIQUE constraint failed: items.name"), want: ErrDuplicateKey},
		{name: "validation failed", err: errors.New("validation failed: name"), want: ErrValidation},
		{name: "other error is kept", err: other, want: other},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := HandleGormError("op", tt.err)
			require.Error(t, got)
			assert.ErrorIs(t, got, tt.want)
			assert.Contains(t, got.Error(), "op: ")
		})
	}

	t.Run("nil is nil", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, HandleGormError("op", nil))
	})
}
