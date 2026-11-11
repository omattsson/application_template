package models

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrTooManyAPIKeys: the user already has the maximum number of active (not
// expired) API keys (APIKeyRepository.CreateLimited).
var ErrTooManyAPIKeys = errors.New("too many active API keys")

// APIKeyFilter selects API keys for APIKeyRepository.ListPaged. A zero field
// does not filter.
type APIKeyFilter struct {
	// UserID matches the owner exactly.
	UserID string
}

// APIKeyPrefixLen is the number of leading characters of a raw API key that
// are stored in clear (APIKey.Prefix) for display and lookup.
const APIKeyPrefixLen = 16

// MaxAPIKeyNameLen is the size of api_keys.name.
const MaxAPIKeyNameLen = 255

// apiKeyBytes is the number of random bytes of a key (64 hex characters).
const apiKeyBytes = 32

// APIKey is a user-generated key for programmatic access. The raw key is
// shown once at creation and never stored: KeyHash is its SHA-256 hex digest
// and is never serialized to JSON.
type APIKey struct {
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	ID         string     `json:"id" gorm:"primaryKey;size:36"`
	UserID     string     `json:"user_id" gorm:"size:36;not null;index:idx_api_keys_user_id"`
	Name       string     `json:"name" gorm:"size:255;not null"`
	KeyHash    string     `json:"-" gorm:"size:64;not null"`
	// Prefix is the first APIKeyPrefixLen characters of the raw key.
	Prefix string `json:"prefix" gorm:"size:20;not null;index:idx_api_keys_prefix"`
}

// APIKeyRepository defines data access operations for API keys. A missing
// key is dberrors.ErrNotFound and an invalid one dberrors.ErrValidation.
type APIKeyRepository interface {
	// Create validates the key (a 64-character hex hash, never a raw key)
	// and sets ID (when empty) and CreatedAt.
	Create(ctx context.Context, key *APIKey) error
	// FindByID returns the key keyID of userID; a key of another user is
	// not found.
	FindByID(ctx context.Context, userID, keyID string) (*APIKey, error)
	// FindByIDs returns the keys with these IDs that exist (of any user), in
	// no defined order. Missing IDs are not an error; no IDs return nil.
	FindByIDs(ctx context.Context, keyIDs []string) ([]*APIKey, error)
	// FindByPrefix returns the keys with this prefix (ErrNotFound for none).
	// The caller verifies the raw key against each KeyHash.
	FindByPrefix(ctx context.Context, prefix string) ([]*APIKey, error)
	// ListByUser returns the keys of a user, newest first.
	ListByUser(ctx context.Context, userID string) ([]*APIKey, error)
	// ListPaged returns one page of the keys that match filter, newest
	// first, without the hash, and the number of all matches. limit <= 0
	// returns all matches; offset applies only with a limit.
	ListPaged(ctx context.Context, filter APIKeyFilter, limit, offset int) ([]*APIKey, int64, error)
	// CreateLimited is Create with a cap: in one transaction that locks the
	// owner row it counts the owner's keys that have not expired at now and
	// returns ErrTooManyAPIKeys when there are already maxActive. An owner
	// that does not exist is dberrors.ErrNotFound; maxActive < 1 is
	// dberrors.ErrValidation.
	CreateLimited(ctx context.Context, key *APIKey, maxActive int, now time.Time) error
	// UpdateLastUsed sets last_used_at. A key that exists is a success even
	// when the row did not change; a missing key is dberrors.ErrNotFound.
	UpdateLastUsed(ctx context.Context, userID, keyID string, t time.Time) error
	Delete(ctx context.Context, userID, keyID string) error
	// DeleteAllForUser removes every API key of the user in one statement and
	// returns the number of deleted keys. Zero keys is not an error.
	DeleteAllForUser(ctx context.Context, userID string) (int64, error)
}

// GenerateAPIKey creates a cryptographically random 32-byte key and returns:
//   - rawKey: 64-character hex string, returned to the user once
//   - prefix: the first APIKeyPrefixLen characters, for display and lookup
//   - hash: SHA-256 hex digest of rawKey, the only form that is stored
func GenerateAPIKey() (rawKey, prefix, hash string, err error) {
	b := make([]byte, apiKeyBytes)
	if _, err = rand.Read(b); err != nil {
		return "", "", "", fmt.Errorf("generate api key: %w", err)
	}
	rawKey = hex.EncodeToString(b)
	return rawKey, rawKey[:APIKeyPrefixLen], HashAPIKey(rawKey), nil
}

// APIKeyRawPrefix starts every API key that a client sees ("sk_<64 hex>"). It
// is not secret: it lets a person and a secret scanner recognise a key. The
// stored prefix and hash are of the 64 hex characters after it.
const APIKeyRawPrefix = "sk_"

// FormatAPIKey returns the form of rawKey (from GenerateAPIKey) that a
// client sends in the X-API-Key header: APIKeyRawPrefix + rawKey.
func FormatAPIKey(rawKey string) string { return APIKeyRawPrefix + rawKey }

// ParseAPIKey returns the raw key of a client key ("sk_" and exactly 64
// lower-case hex characters). Any other value is not a key of this
// application, so it is refused without a database lookup.
func ParseAPIKey(clientKey string) (rawKey string, ok bool) {
	raw, found := strings.CutPrefix(clientKey, APIKeyRawPrefix)
	if !found || len(raw) != apiKeyBytes*2 {
		return "", false
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return raw, true
}

// HashAPIKey returns the SHA-256 hex digest of a raw API key. A key is 32
// random bytes, so a fast hash is enough; a password needs a slow one.
func HashAPIKey(rawKey string) string {
	sum := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(sum[:])
}

// VerifyAPIKeyHash reports whether rawKey matches the stored hash, in
// constant time.
func VerifyAPIKeyHash(rawKey, storedHash string) bool {
	if rawKey == "" || storedHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(HashAPIKey(rawKey)), []byte(storedHash)) == 1
}
