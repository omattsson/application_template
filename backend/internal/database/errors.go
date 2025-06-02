package database

import (
	"errors"
	"fmt"
)

// Common database errors
var (
	ErrNotFound         = errors.New("record not found")
	ErrDuplicateKey     = errors.New("duplicate key violation")
	ErrValidation       = errors.New("validation error")
	ErrConnectionFailed = errors.New("database connection failed")
)

// DatabaseError wraps database-specific errors with additional context
type DatabaseError struct {
	Op  string
	Err error
}

func (e *DatabaseError) Error() string {
	if e.Op != "" {
		return fmt.Sprintf("%s: %v", e.Op, e.Err)
	}
	return e.Err.Error()
}

func (e *DatabaseError) Unwrap() error {
	return e.Err
}

// NewDatabaseError creates a new database error with operation context
func NewDatabaseError(op string, err error) error {
	if err == nil {
		return nil
	}
	return &DatabaseError{Op: op, Err: err}
}
