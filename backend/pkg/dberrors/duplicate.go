package dberrors

import (
	"errors"
	"strings"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// mysqlErrDuplicateEntry is the MySQL error number of a duplicate key.
const mysqlErrDuplicateEntry = 1062

// sqliteUniqueFailed starts the message of a SQLite unique constraint error.
// The SQLite driver (unit tests) has no error type that this package can
// import without cgo, so its message is matched.
const sqliteUniqueFailed = "UNIQUE constraint failed"

// IsDuplicateKey reports whether err is a duplicate key violation, also when
// wrapped: MySQL error 1062, a SQLite unique constraint error, GORM's
// translated gorm.ErrDuplicatedKey, or the ErrDuplicateKey sentinel.
//
// A MySQL error with another number is never a duplicate, whatever its text,
// and the MySQL message text alone ("Duplicate entry") is not matched.
func IsDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrDuplicateKey) || errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == mysqlErrDuplicateEntry
	}
	return strings.Contains(err.Error(), sqliteUniqueFailed)
}
