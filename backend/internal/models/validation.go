package models

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Compile email regex once at package level for performance.
var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)

var (
	ErrInvalidEmail    = errors.New("invalid email format")
	ErrEmptyUsername   = errors.New("username cannot be empty")
	ErrUsernameTooLong = fmt.Errorf("username must be at most %d characters", MaxUsernameLen)
	ErrInvalidRole     = errors.New("role must be one of admin, devops, user")
	ErrInvalidPrice    = errors.New("price must be positive")
	ErrEmptyItemName   = errors.New("item name cannot be empty")
)

// sha256HexLen is the length of a SHA-256 hex digest.
const sha256HexLen = 64

// Validate implements model validation for User: a username, a known role
// and, when set, a well-formed email.
func (u *User) Validate() error {
	if strings.TrimSpace(u.Username) == "" {
		return ErrEmptyUsername
	}
	if len(u.Username) > MaxUsernameLen {
		return ErrUsernameTooLong
	}
	if !IsValidRole(u.Role) {
		return ErrInvalidRole
	}
	if u.Email != "" && !IsValidEmail(u.Email) {
		return ErrInvalidEmail
	}
	return nil
}

// IsValidEmail reports whether email is a well-formed address.
func IsValidEmail(email string) bool {
	return emailRegex.MatchString(email)
}

// Validate implements model validation for Item.
func (i *Item) Validate() error {
	if i.Name == "" {
		return ErrEmptyItemName
	}

	if i.Price <= 0 {
		return ErrInvalidPrice
	}

	return nil
}

// Validate implements model validation for RefreshToken. TokenHash must be
// a SHA-256 hex digest, so a raw token is never stored by mistake.
func (t *RefreshToken) Validate() error {
	if t.ID == "" {
		return errors.New("id is required")
	}
	if t.UserID == "" {
		return errors.New("user_id is required")
	}
	if t.TokenHash == "" {
		return errors.New("token_hash is required")
	}
	if !isSHA256Hex(t.TokenHash) {
		return errors.New("token_hash must be a 64-character hex string")
	}
	if t.ExpiresAt.IsZero() {
		return errors.New("expires_at is required")
	}
	if t.LastActivity.IsZero() {
		return errors.New("last_activity is required")
	}
	return nil
}

// Validate implements model validation for APIKey. KeyHash must be a
// SHA-256 hex digest, so a raw key is never stored by mistake.
func (k *APIKey) Validate() error {
	if k.UserID == "" {
		return errors.New("user_id is required")
	}
	if strings.TrimSpace(k.Name) == "" {
		return errors.New("name is required")
	}
	if len(k.Name) > MaxAPIKeyNameLen {
		return fmt.Errorf("name must be at most %d characters", MaxAPIKeyNameLen)
	}
	if !isSHA256Hex(k.KeyHash) {
		return errors.New("key_hash must be a 64-character hex string")
	}
	if k.Prefix == "" {
		return errors.New("prefix is required")
	}
	return nil
}

// Validate implements model validation for AuditLog.
func (a *AuditLog) Validate() error {
	if a.UserID == "" {
		return errors.New("user_id is required")
	}
	if a.Action == "" {
		return errors.New("action is required")
	}
	if a.EntityType == "" {
		return errors.New("entity_type is required")
	}
	if len(a.EntityID) > MaxAuditEntityIDLen {
		return fmt.Errorf("entity_id must be at most %d characters", MaxAuditEntityIDLen)
	}
	return nil
}

// isSHA256Hex reports whether s is a lower-case SHA-256 hex digest.
func isSHA256Hex(s string) bool {
	if len(s) != sha256HexLen || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
