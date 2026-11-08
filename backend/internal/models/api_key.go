package models

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"time"
)

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
