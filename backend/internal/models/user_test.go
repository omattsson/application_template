package models

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsSSOProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		want     bool
	}{
		{provider: AuthProviderOIDC, want: true},
		{provider: AuthProviderLocal, want: false},
		{provider: "", want: false},
		{provider: "OIDC", want: false},
		{provider: "saml", want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.provider, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, IsSSOProvider(tt.provider))
		})
	}
}

func TestUserValidateRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		role  string
		valid bool
	}{
		{role: RoleAdmin, valid: true},
		{role: RoleDevOps, valid: true},
		{role: RoleUser, valid: true},
		{role: "", valid: false},
		{role: "Admin", valid: false},
		{role: "developer", valid: false},
		{role: "root", valid: false},
		{role: " admin", valid: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run("role="+tt.role, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.valid, IsValidRole(tt.role))

			u := &User{Username: "alice", Role: tt.role}
			err := u.Validate()
			if tt.valid {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrInvalidRole)
			}
		})
	}
}

func TestUserValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		user    User
		wantErr error
	}{
		{name: "minimal", user: User{Username: "alice", Role: RoleUser}},
		{name: "with email", user: User{Username: "alice", Role: RoleUser, Email: "alice@example.com"}},
		{name: "empty username", user: User{Role: RoleUser}, wantErr: ErrEmptyUsername},
		{name: "blank username", user: User{Username: "   ", Role: RoleUser}, wantErr: ErrEmptyUsername},
		{name: "username too long", user: User{Username: strings.Repeat("a", MaxUsernameLen+1), Role: RoleUser}, wantErr: ErrUsernameTooLong},
		{name: "username at the limit", user: User{Username: strings.Repeat("a", MaxUsernameLen), Role: RoleUser}},
		{name: "bad email", user: User{Username: "alice", Role: RoleUser, Email: "not-an-email"}, wantErr: ErrInvalidEmail},
		{name: "bad role", user: User{Username: "alice", Role: "superuser"}, wantErr: ErrInvalidRole},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.user.Validate()
			if tt.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestUserJSONHidesPasswordHash(t *testing.T) {
	t.Parallel()
	ext := "sub-1"
	u := User{ID: "u1", Username: "alice", PasswordHash: "secret-hash", Role: RoleAdmin, ExternalID: &ext}

	b, err := json.Marshal(u)
	require.NoError(t, err)

	assert.NotContains(t, string(b), "secret-hash")
	assert.NotContains(t, string(b), "password")
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	for _, key := range []string{"id", "username", "email", "display_name", "role", "auth_provider", "external_id", "disabled", "service_account", "created_at", "updated_at"} {
		assert.Contains(t, m, key)
	}
}
