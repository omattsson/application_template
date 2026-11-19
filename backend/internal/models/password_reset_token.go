package models

import (
	"context"
	"time"
)

// PasswordResetTokenHashLen is the length of PasswordResetToken.TokenHash: the
// hex SHA-256 of the raw token.
const PasswordResetTokenHashLen = 64

// PasswordResetToken is one issued reset token (table password_reset_tokens,
// optional module PASSWORD_RESET_ENABLED). Only the SHA-256 of the raw token is
// stored; the raw token travels in the mail and is never stored or logged. Its
// definition is copied into migration 20261119000001 (passwordResetTokenV1); a
// change here needs a new migration, and TestPasswordResetTokensMatchModel
// fails without one.
//
// Every time is written and compared with the database clock, never with the
// clock of a replica. The repository never reads the times into Go.
type PasswordResetToken struct {
	// ExpiresAt is the end of the token's life (database clock).
	ExpiresAt time.Time `gorm:"column:expires_at;type:datetime(3);not null;index:idx_password_reset_tokens_expires_at"`
	// CreatedAt is the time of the insert (database clock).
	CreatedAt time.Time `gorm:"column:created_at;type:datetime(3);not null"`
	// UsedAt is the time the token was consumed (database clock); NULL while
	// the token is unused.
	UsedAt *time.Time `gorm:"column:used_at;type:datetime(3)"`
	// ID is a UUID.
	ID string `gorm:"column:id;primaryKey;size:36"`
	// UserID is the account that the token resets.
	UserID string `gorm:"column:user_id;size:36;not null;index:idx_password_reset_tokens_user_id"`
	// TokenHash is the hex SHA-256 of the raw token.
	TokenHash string `gorm:"column:token_hash;size:64;not null;uniqueIndex:idx_password_reset_tokens_token_hash"`
}

// TableName returns the table name of PasswordResetToken.
func (PasswordResetToken) TableName() string { return "password_reset_tokens" }

// PasswordResetTokenRepository stores the reset tokens. MySQL is the arbiter:
// every decision is one atomic statement or one short transaction, and every
// time is read from the database clock.
type PasswordResetTokenRepository interface {
	// Issue stores a new token for userID, valid for ttl, unless the user got a
	// token less than minInterval ago (then it stores nothing and returns
	// false). It deletes the unused earlier tokens of the user in the same
	// transaction, so at most one unused token per user exists. tokenHash must
	// be a 64-character hex digest (ErrValidation otherwise). Two requests for
	// one user at the same time issue one token.
	Issue(ctx context.Context, userID, tokenHash string, ttl, minInterval time.Duration) (issued bool, err error)
	// Consume marks the token with this hash as used, when it is unused and
	// not expired, in one conditional UPDATE, and returns the user it belongs
	// to. A token that does not exist, is used or is expired gives ok=false
	// and no error. Of two requests with the same token exactly one gets
	// ok=true.
	Consume(ctx context.Context, tokenHash string) (userID string, ok bool, err error)
	// DeleteUnusedForUser deletes the unused tokens of a user and returns the
	// number deleted.
	DeleteUnusedForUser(ctx context.Context, userID string) (int64, error)
	// DeleteStale deletes at most limit tokens that expired, or that were used
	// more than usedRetention ago (database time), and returns the number
	// deleted.
	DeleteStale(ctx context.Context, usedRetention time.Duration, limit int) (int64, error)
}
