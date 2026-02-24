// Package database provides database connectivity, migrations, and repository factories.
// Error types are re-exported from pkg/dberrors to maintain a single source of truth.
package database

import "backend/pkg/dberrors"

// Re-export error types from pkg/dberrors for backward compatibility.
type DatabaseError = dberrors.DatabaseError

var (
	ErrNotFound         = dberrors.ErrNotFound
	ErrDuplicateKey     = dberrors.ErrDuplicateKey
	ErrValidation       = dberrors.ErrValidation
	ErrConnectionFailed = dberrors.ErrConnectionFailed
	NewDatabaseError    = dberrors.NewDatabaseError
)
