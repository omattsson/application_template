package models

import (
	"context"
	"time"
)

// RefreshToken is a server-side refresh token for access-token rotation.
// The raw token is never stored: only its SHA-256 hex digest (TokenHash),
// which is never serialized to JSON.
//
// Tokens of one login session form a family: rotation copies FamilyID and
// SessionStartedAt to the new token. FamilyID is the ID of the first token of
// the session and is also the "sid" claim of the access tokens of the
// session. A token without a FamilyID is its own family, and one without a
// SessionStartedAt started at CreatedAt; use SessionFamily and SessionStart.
type RefreshToken struct {
	ExpiresAt    time.Time `json:"expires_at" gorm:"not null;index:idx_refresh_tokens_expires_at"`
	LastActivity time.Time `json:"last_activity" gorm:"not null"`
	CreatedAt    time.Time `json:"created_at"`
	// SessionStartedAt is the login time of the session (absolute lifetime).
	SessionStartedAt time.Time `json:"session_started_at"`
	// RotatedAt is set only when a normal rotation consumes the token. Logout
	// and revoke-all leave it nil, so such a token never gets the reuse grace.
	RotatedAt *time.Time `json:"rotated_at,omitempty"`
	ID        string     `json:"id" gorm:"primaryKey;size:36"`
	UserID    string     `json:"user_id" gorm:"size:36;not null;index:idx_refresh_tokens_user_id"`
	TokenHash string     `json:"-" gorm:"size:64;not null;uniqueIndex:idx_refresh_tokens_token_hash"`
	FamilyID  string     `json:"family_id" gorm:"size:36;index:idx_refresh_tokens_family_id"`
	UserAgent string     `json:"user_agent" gorm:"size:500"`
	IPAddress string     `json:"ip_address" gorm:"size:45"`
	Revoked   bool       `json:"revoked" gorm:"not null;default:false"`
}

// SessionFamily returns the family ID of the token. A token without a
// family is its own family.
func (t *RefreshToken) SessionFamily() string {
	if t.FamilyID != "" {
		return t.FamilyID
	}
	return t.ID
}

// SessionStart returns the start of the session. A token without a session
// start uses its creation time.
func (t *RefreshToken) SessionStart() time.Time {
	if !t.SessionStartedAt.IsZero() {
		return t.SessionStartedAt
	}
	return t.CreatedAt
}

// MaxRecentFamilies bounds RefreshTokenRepository.ListRecentFamiliesForUser.
const MaxRecentFamilies = 1000

// RefreshTokenRepository defines data access operations for refresh tokens.
// Every method runs its queries with ctx. A missing token is
// dberrors.ErrNotFound and a duplicate hash dberrors.ErrDuplicateKey.
type RefreshTokenRepository interface {
	// Create validates the token (a 64-character hex hash, never a raw
	// token) and sets ID (when empty), FamilyID (when empty: ID, its own
	// family), CreatedAt (when zero) and SessionStartedAt (when zero:
	// CreatedAt, a new session).
	Create(ctx context.Context, token *RefreshToken) error
	FindByTokenHash(ctx context.Context, hash string) (*RefreshToken, error)
	RevokeByID(ctx context.Context, id string) error
	// MarkRotatedIfActive revokes the token and sets RotatedAt, but only when
	// the token is still active. It returns the number of rows changed (0 when
	// another request already consumed or revoked the token).
	MarkRotatedIfActive(ctx context.Context, id string, rotatedAt time.Time) (int64, error)
	// ClaimReuseGrace spends the reuse grace of a consumed token: it clears
	// RotatedAt, but only when a rotation consumed the token at or after
	// rotatedSince. It returns true for the one caller that cleared it; every
	// later claim (and a token revoked without rotation) returns false.
	ClaimReuseGrace(ctx context.Context, id string, rotatedSince time.Time) (bool, error)
	// RevokeFamily revokes all active tokens of a session family (including a
	// token without a family whose own ID is the family ID).
	RevokeFamily(ctx context.Context, familyID string) error
	// RevokeFamilyForUser is RevokeFamily limited to tokens of userID: a
	// family of another user is left alone.
	RevokeFamilyForUser(ctx context.Context, familyID, userID string) error
	// ListRecentFamiliesForUser returns the distinct session family IDs of the
	// user's refresh tokens that expire after since, revoked or not, newest
	// family first, at most MaxRecentFamilies. A token without a family
	// returns its own ID.
	ListRecentFamiliesForUser(ctx context.Context, userID string, since time.Time) ([]string, error)
	// CountActiveInFamily counts the tokens of a family that are not revoked
	// and not expired.
	CountActiveInFamily(ctx context.Context, familyID string) (int64, error)
	// TouchFamily sets LastActivity to at for the active tokens of a family.
	// It never moves LastActivity backwards.
	TouchFamily(ctx context.Context, familyID string, at time.Time) error
	RevokeAllForUser(ctx context.Context, userID string) error
	RevokeAllForUserExcept(ctx context.Context, userID string, excludeID string) error
	// RevokeAllForUserExceptFamily revokes every active token of the user
	// except the tokens of the session family familyID (a token without a
	// family counts as its own family). An empty familyID keeps nothing, so
	// it revokes every active token of the user. The self-service password
	// change uses it to end the other sessions and keep the current one.
	RevokeAllForUserExceptFamily(ctx context.Context, userID string, familyID string) error
	// RevokeOldestForUser revokes the active tokens of the user except the
	// keep newest ones (by CreatedAt) and the token excludeID (empty: none).
	// It never revokes a token that is newer than the kept ones, so a login
	// that runs at the same time keeps its new session. It returns the
	// number of revoked tokens.
	RevokeOldestForUser(ctx context.Context, userID string, keep int, excludeID string) (int64, error)
	// DeleteExpired deletes the tokens that expired before `before`. The
	// cleanup passes a time in the past, so a row stays while its session can
	// still hold a live access token and logout-all can list it.
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
	CountActiveForUser(ctx context.Context, userID string) (int64, error)
	// WithTx executes fn within a database transaction that runs with ctx.
	// The repository passed to fn shares the same transaction; if fn returns
	// an error the transaction is rolled back. After a deadlock or lock wait
	// timeout the whole transaction, fn included, runs again, so fn must
	// only write through the repository it gets. When ctx ends, the
	// transaction commits nothing and the error matches the context error.
	WithTx(ctx context.Context, fn func(txRepo RefreshTokenRepository) error) error
}
