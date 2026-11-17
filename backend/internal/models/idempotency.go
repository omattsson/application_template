package models

import (
	"context"
	"time"
)

// IdempotencyScopeShared is the scope of every key with AUTH_ENABLED=false:
// the API has no users then, so all clients share one key space.
const IdempotencyScopeShared = "*"

// IdempotencyKey is one Idempotency-Key of one scope (table
// idempotency_keys, optional module IDEMPOTENCY_ENABLED). The row is the
// claim of the first request with the key. It starts in progress
// (StatusCode 0) and holds the stored response once the request finished.
// Its definition is copied into migration 20261117000001
// (idempotencyKeyV1); a change here needs a new migration, and
// TestIdempotencyKeysMatchModel fails without one.
//
// Every time is written and compared with the database clock, never with the
// clock of a replica. The repository never reads the times into Go.
type IdempotencyKey struct {
	// ExpiresAt is the end of the key's life (database clock). The janitor
	// deletes the row after it, and a request that finds the row after it
	// takes the key over.
	ExpiresAt time.Time `gorm:"column:expires_at;type:datetime(3);not null;index:idx_idempotency_keys_expires_at"`
	// LockedAt is the time of the claim (database clock). An in-progress row
	// older than the lock timeout can be taken over.
	LockedAt time.Time `gorm:"column:locked_at;type:datetime(3);not null"`
	// Scope is the user ID with auth on and IdempotencyScopeShared otherwise.
	Scope string `gorm:"column:scope;primaryKey;size:64"`
	// Key is the client's Idempotency-Key (at most 255 bytes, printable ASCII).
	Key string `gorm:"column:idempotency_key;primaryKey;size:255"`
	// RequestHash is the hex SHA-256 of method, path, query and body.
	RequestHash string `gorm:"column:request_hash;size:64;not null"`
	// LockID identifies the request that owns the row. Only that request may
	// store a response or release the key, so a request that was taken over
	// cannot overwrite the work of its successor.
	LockID string `gorm:"column:lock_id;size:36;not null"`
	// ContentType is the Content-Type of the stored response.
	ContentType string `gorm:"column:content_type;size:255;not null"`
	// Body is the stored response body; NULL while the request runs.
	Body []byte `gorm:"column:body;type:mediumblob"`
	// StatusCode is the status of the stored response; 0 while in progress.
	StatusCode int `gorm:"column:status_code;type:smallint;not null"`
}

// TableName returns the table name of IdempotencyKey.
func (IdempotencyKey) TableName() string { return "idempotency_keys" }

// IdempotencyOutcome is what Claim found.
type IdempotencyOutcome int

const (
	// IdempotencyClaimed: the caller owns the key and runs the request.
	IdempotencyClaimed IdempotencyOutcome = iota + 1
	// IdempotencyReplay: a finished request with the same fingerprint exists;
	// Response holds its answer.
	IdempotencyReplay
	// IdempotencyMismatch: the key belongs to a request with another
	// fingerprint (method, path, query or body).
	IdempotencyMismatch
	// IdempotencyInProgress: the first request with the key is still running
	// and its lock has not timed out. RetryAfter says how long it may block.
	IdempotencyInProgress
)

// IdempotencyResponse is the stored answer of a finished request.
type IdempotencyResponse struct {
	ContentType string
	Body        []byte
	StatusCode  int
}

// IdempotencyClaim is the result of IdempotencyRepository.Claim.
type IdempotencyClaim struct {
	// Response is set with IdempotencyReplay.
	Response *IdempotencyResponse
	// LockID is set with IdempotencyClaimed; pass it to Complete or Release.
	LockID string
	// RetryAfter is set with IdempotencyInProgress: the time until the lock
	// can be taken over (at least one second).
	RetryAfter time.Duration
	Outcome    IdempotencyOutcome
}

// IdempotencyRepository stores the keys. All times are written and compared
// in SQL with the database clock. MySQL is the arbiter between replicas:
// every decision is one atomic statement on the row of (scope, key).
type IdempotencyRepository interface {
	// Claim tries to take the key for a request with fingerprint hash.
	// A missing or expired row is claimed. A row that is in progress past
	// lockTimeout with the same hash is taken over (one caller wins). Any other
	// row is reported as IdempotencyReplay, IdempotencyMismatch or
	// IdempotencyInProgress. ttl is the life of a new claim.
	Claim(ctx context.Context, scope, key, hash string, ttl, lockTimeout time.Duration) (*IdempotencyClaim, error)
	// Complete stores the response and restarts the life of the key at ttl. It
	// returns false, and changes nothing, when lockID no longer owns the row
	// (taken over, expired and deleted).
	Complete(ctx context.Context, scope, key, lockID string, resp IdempotencyResponse, ttl time.Duration) (bool, error)
	// Release deletes the in-progress row that lockID owns so that a client can
	// retry. A row of another owner, or a finished row, is left alone.
	Release(ctx context.Context, scope, key, lockID string) error
	// DeleteExpired deletes up to limit rows whose life ended and returns the
	// number deleted.
	DeleteExpired(ctx context.Context, limit int) (int64, error)
}
