package database

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"backend/pkg/dberrors"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRepoError(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	validation := validationError("create", errors.New("name is required"))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name      string
		ctx       context.Context
		err       error
		wantIs    []error
		wantNotIs []error
		wantSame  bool
		deny      string
	}{
		{name: "nil", ctx: context.Background()},
		{name: "not found", ctx: context.Background(), err: gorm.ErrRecordNotFound, wantIs: []error{dberrors.ErrNotFound}},
		{
			name: "MySQL duplicate keeps no value", ctx: context.Background(),
			err:    &mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'alice' for key 'users.idx_users_username'"},
			wantIs: []error{dberrors.ErrDuplicateKey}, deny: "alice",
		},
		{name: "SQLite duplicate", ctx: context.Background(), err: errors.New("UNIQUE constraint failed: users.username"), wantIs: []error{dberrors.ErrDuplicateKey}},
		{name: "a DatabaseError is kept", ctx: context.Background(), err: validation, wantIs: []error{dberrors.ErrValidation}, wantSame: true},
		{name: "other error is wrapped", ctx: context.Background(), err: errBoom, wantIs: []error{errBoom}, wantNotIs: []error{dberrors.ErrNotFound, dberrors.ErrDuplicateKey}},
		{name: "done context is added", ctx: cancelled, err: fmt.Errorf("driver: %w", errBoom), wantIs: []error{context.Canceled, errBoom}},
		{name: "context error is not added twice", ctx: cancelled, err: context.Canceled, wantIs: []error{context.Canceled}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := repoError(tt.ctx, "op", tt.err)
			if tt.err == nil {
				require.NoError(t, got)
				return
			}
			require.Error(t, got)
			var dbErr *dberrors.DatabaseError
			require.ErrorAs(t, got, &dbErr, "the result is (or wraps) a DatabaseError")
			for _, want := range tt.wantIs {
				require.ErrorIs(t, got, want)
			}
			for _, notWant := range tt.wantNotIs {
				require.NotErrorIs(t, got, notWant)
			}
			if tt.wantSame {
				assert.Same(t, tt.err, got)
			}
			if tt.deny != "" {
				assert.NotContains(t, got.Error(), tt.deny)
			}
		})
	}
}
