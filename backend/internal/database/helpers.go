package database

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"backend/pkg/dberrors"

	"gorm.io/gorm"
)

// IsDuplicateKey reports whether err is a duplicate key violation: MySQL
// error 1062 or a SQLite unique constraint error, also when wrapped. It is
// the one matcher that the repositories (dberrors.ErrDuplicateKey, 409) and
// the GORM logger share; see dberrors.IsDuplicateKey.
func IsDuplicateKey(err error) bool {
	return dberrors.IsDuplicateKey(err)
}

// repoError maps an error of the repository operation op: a missing row to
// ErrNotFound, a duplicate key to ErrDuplicateKey (409; the duplicate value
// is not kept), both in a *DatabaseError. A *DatabaseError is returned as
// is. Any other error is wrapped in a *DatabaseError so errors.Is still
// finds it. When ctx is done, the result also matches the context error, so
// a cancelled request maps to 499 even when the driver returned another
// error.
func repoError(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		err = fmt.Errorf("%w: %w", ctxErr, err)
	}
	if _, ok := errors.AsType[*dberrors.DatabaseError](err); ok {
		return err
	}
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return dberrors.NewDatabaseError(op, dberrors.ErrNotFound)
	case IsDuplicateKey(err):
		return dberrors.NewDatabaseError(op, dberrors.ErrDuplicateKey)
	default:
		return dberrors.NewDatabaseError(op, err)
	}
}

// validationError returns the validation failure of op as ErrValidation with
// the validator's message.
func validationError(op string, err error) error {
	return dberrors.NewDatabaseError(op, fmt.Errorf("%w: %s", dberrors.ErrValidation, err.Error()))
}

// likeEscaper escapes the LIKE wildcards. "!" is the escape character, which
// works the same on MySQL and SQLite (a backslash needs different quoting).
var likeEscaper = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")

// containsPattern returns a LIKE pattern (with ESCAPE '!') that matches s
// anywhere, with % and _ in s as literals.
func containsPattern(s string) string {
	return "%" + likeEscaper.Replace(s) + "%"
}
