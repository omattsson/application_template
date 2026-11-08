package database

import "backend/pkg/dberrors"

// IsDuplicateKey reports whether err is a duplicate key violation: MySQL
// error 1062 or a SQLite unique constraint error, also when wrapped. It is
// the one matcher that the repositories (dberrors.ErrDuplicateKey, 409) and
// the GORM logger share; see dberrors.IsDuplicateKey.
func IsDuplicateKey(err error) bool {
	return dberrors.IsDuplicateKey(err)
}
