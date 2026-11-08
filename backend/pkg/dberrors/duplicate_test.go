package dberrors

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"gorm.io/gorm"
)

func TestIsDuplicateKey(t *testing.T) {
	t.Parallel()

	mysqlDup := &mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'x' for key 'users.idx_users_username'"}
	sqliteDup := errors.New("UNIQUE constraint failed: users.username")

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "MySQL 1062", err: mysqlDup, want: true},
		{name: "wrapped MySQL 1062", err: fmt.Errorf("create: %w", mysqlDup), want: true},
		{name: "MySQL 1062 in a DatabaseError", err: NewDatabaseError("create", mysqlDup), want: true},
		{name: "SQLite unique constraint", err: sqliteDup, want: true},
		{name: "wrapped SQLite unique constraint", err: fmt.Errorf("create: %w", sqliteDup), want: true},
		{name: "GORM translated duplicate", err: gorm.ErrDuplicatedKey, want: true},
		{name: "duplicate sentinel", err: NewDatabaseError("create", ErrDuplicateKey), want: true},
		{name: "MySQL deadlock", err: &mysql.MySQLError{Number: 1213, Message: "Deadlock found"}, want: false},
		{name: "MySQL other error that mentions a unique constraint", err: &mysql.MySQLError{Number: 1366, Message: "UNIQUE constraint failed"}, want: false},
		{name: "text only, MySQL style", err: errors.New("Duplicate entry 'x' for key 'y'"), want: false},
		{name: "other error", err: errors.New("bad connection"), want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, IsDuplicateKey(tt.err))
		})
	}
}

func TestHandleGormErrorDuplicate(t *testing.T) {
	t.Parallel()

	err := HandleGormError("create", &mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'x' for key 'k'"})
	assert.ErrorIs(t, err, ErrDuplicateKey)

	err = HandleGormError("create", errors.New("UNIQUE constraint failed: t.c"))
	assert.ErrorIs(t, err, ErrDuplicateKey)

	err = HandleGormError("create", errors.New("Duplicate entry 'x' for key 'k'"))
	assert.NotErrorIs(t, err, ErrDuplicateKey, "the text alone is not a typed duplicate")
}
