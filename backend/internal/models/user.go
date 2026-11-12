package models

import (
	"context"
	"errors"
	"time"
)

// User roles, lowest to highest permission.
const (
	RoleUser   = "user"
	RoleDevOps = "devops"
	RoleAdmin  = "admin"
)

// Auth providers of a user. A local user signs in with a password; an OIDC
// user signs in through the identity provider and has an ExternalID.
const (
	AuthProviderLocal = "local"
	AuthProviderOIDC  = "oidc"
)

// IsSSOProvider reports whether provider is an identity provider that a user
// can be linked to (AuthProviderOIDC). The empty string, AuthProviderLocal and
// any unknown name are not.
func IsSSOProvider(provider string) bool {
	return provider == AuthProviderOIDC
}

// MaxUsernameLen is the size of users.username. MaxExternalIDLen is the size
// of users.external_id.
const (
	MaxUsernameLen   = 100
	MaxExternalIDLen = 255
)

// Errors of a link to an identity provider (UserRepository.UpdateProfile).
var (
	// ErrInvalidAuthProvider: the provider to link to is empty, local or
	// unknown.
	ErrInvalidAuthProvider = errors.New("auth provider must be a known identity provider")
	// ErrInvalidExternalID: the external ID is missing, empty or longer than
	// MaxExternalIDLen, or is set without a provider.
	ErrInvalidExternalID = errors.New("external id must be set with a provider and at most 255 characters")
)

// IsValidRole reports whether role is one of RoleUser, RoleDevOps or RoleAdmin.
func IsValidRole(role string) bool {
	switch role {
	case RoleUser, RoleDevOps, RoleAdmin:
		return true
	}
	return false
}

// Errors of the guarded user changes (UpdateRole, SetDisabled, DeleteGuarded).
var (
	// ErrLastAdmin: the change would leave no enabled admin.
	ErrLastAdmin = errors.New("last enabled admin")
	// ErrCallerNotAdmin: the caller is not (or no longer) an enabled admin.
	ErrCallerNotAdmin = errors.New("caller is not an enabled admin")
)

// User is an account that can sign in. The ID is a UUID that the repository
// sets. PasswordHash is never serialized to JSON.
type User struct {
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// ExternalID is the subject at the identity provider; nil for a local
	// user. (auth_provider, external_id) is unique; MySQL allows many NULLs.
	ExternalID   *string `json:"external_id" gorm:"size:255;uniqueIndex:idx_users_auth_provider_external_id,priority:2"`
	ID           string  `json:"id" gorm:"primaryKey;size:36"`
	Username     string  `json:"username" gorm:"size:100;not null;uniqueIndex:idx_users_username"`
	PasswordHash string  `json:"-" gorm:"size:255"`
	DisplayName  string  `json:"display_name" gorm:"size:255"`
	Role         string  `json:"role" gorm:"size:50;not null;index:idx_users_role"`
	AuthProvider string  `json:"auth_provider" gorm:"size:50;not null;default:local;uniqueIndex:idx_users_auth_provider_external_id,priority:1"`
	Email        string  `json:"email" gorm:"size:255"`
	Disabled     bool    `json:"disabled" gorm:"not null;default:false"`
	// ServiceAccount marks a local account that may still sign in with a
	// password when OIDC is the only interactive login (break-glass).
	ServiceAccount bool `json:"service_account" gorm:"not null;default:false"`
}

// UserProfileUpdate lists the columns that UserRepository.UpdateProfile
// sets. A nil field keeps the stored value.
type UserProfileUpdate struct {
	Email       *string
	DisplayName *string
	// Role is the role from the identity provider. It is written only when
	// the user is (or, with LinkProvider, becomes) an SSO user; the role of a
	// local user changes only through UpdateRole.
	Role *string
	// LinkProvider links a local user to an identity provider: it sets
	// auth_provider to this value and external_id to ExternalID, and clears
	// the password hash.
	LinkProvider *string
	ExternalID   *string
}

// UserFilter selects users for UserRepository.ListPaged. A zero field does
// not filter.
type UserFilter struct {
	// Role matches the role exactly.
	Role string
	// Search matches users whose username, display name or email contains
	// it (case-insensitive on the default MySQL collation). % and _ are
	// literals.
	Search string
}

// UserRepository defines data access operations for users. There is no
// full-row update: each change writes only its own columns, so concurrent
// changes (role, disabled, password, profile) cannot overwrite each other.
//
// Errors: a missing user is dberrors.ErrNotFound, a taken username or
// external ID is dberrors.ErrDuplicateKey, an invalid user or role is
// dberrors.ErrValidation, all wrapped in a *dberrors.DatabaseError.
type UserRepository interface {
	// Create validates the user, sets ID (when empty), CreatedAt and
	// UpdatedAt, and defaults an empty Role to RoleUser and an empty
	// AuthProvider to AuthProviderLocal.
	Create(ctx context.Context, user *User) error
	FindByID(ctx context.Context, id string) (*User, error)
	// FindByIDs returns the users with these IDs by ID, in one query,
	// without the password hash. Missing IDs are not in the map.
	FindByIDs(ctx context.Context, ids []string) (map[string]*User, error)
	FindByUsername(ctx context.Context, username string) (*User, error)
	FindByExternalID(ctx context.Context, provider, externalID string) (*User, error)
	// UpdatePassword sets only the password hash of a user.
	UpdatePassword(ctx context.Context, id, passwordHash string) error
	// UpdateProfile sets only the columns listed in upd. It never writes
	// disabled, and it writes the role only for an SSO user (see
	// UserProfileUpdate.Role). Use it instead of a full-row save, which would
	// overwrite a concurrent role or disabled change.
	UpdateProfile(ctx context.Context, id string, upd UserProfileUpdate) error
	// Delete deletes a user without the last-admin guard. Admin endpoints
	// use DeleteGuarded.
	Delete(ctx context.Context, id string) error
	// ListPaged returns one page of the users that match filter, ordered by
	// username, without the password hash, and the number of all matches.
	// limit <= 0 returns all matches; offset applies only with a limit.
	ListPaged(ctx context.Context, filter UserFilter, limit, offset int) ([]User, int64, error)
	Count(ctx context.Context) (int64, error)
	// CountEnabledAdmins returns the number of enabled admins. It is a plain
	// read, without a lock: a handler uses it for a pre-check that refuses an
	// obviously refused change before any side effect. The guarded changes
	// below stay the authority.
	CountEnabledAdmins(ctx context.Context) (int64, error)

	// Guarded admin changes. Each runs in one transaction that locks the
	// admin rows (ordered by ID) and then the target row, so concurrent
	// changes serialize. Each returns ErrCallerNotAdmin when callerID is not
	// an enabled admin at that moment, and ErrLastAdmin (changing nothing)
	// when the change would leave no enabled admin.

	// UpdateRole sets the role of a user and returns the previous role. An
	// unchanged role is a no-op.
	UpdateRole(ctx context.Context, callerID, id, role string) (oldRole string, err error)
	// SetDisabled disables or enables a user.
	SetDisabled(ctx context.Context, callerID, id string, disabled bool) error
	// DeleteGuarded deletes a user.
	DeleteGuarded(ctx context.Context, callerID, id string) error
}
