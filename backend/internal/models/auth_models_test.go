package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTokenHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestRefreshTokenValidate(t *testing.T) {
	t.Parallel()
	now := time.Now()
	valid := func() RefreshToken {
		return RefreshToken{ID: "rt", UserID: "u1", TokenHash: testTokenHash, ExpiresAt: now, LastActivity: now}
	}

	tests := []struct {
		name    string
		mutate  func(*RefreshToken)
		wantErr string
	}{
		{name: "valid", mutate: func(*RefreshToken) {}},
		{name: "no id", mutate: func(rt *RefreshToken) { rt.ID = "" }, wantErr: "id is required"},
		{name: "no user", mutate: func(rt *RefreshToken) { rt.UserID = "" }, wantErr: "user_id is required"},
		{name: "no hash", mutate: func(rt *RefreshToken) { rt.TokenHash = "" }, wantErr: "token_hash is required"},
		{name: "short hash", mutate: func(rt *RefreshToken) { rt.TokenHash = "abc" }, wantErr: "64-character hex"},
		{name: "non-hex hash", mutate: func(rt *RefreshToken) { rt.TokenHash = strings.Repeat("z", 64) }, wantErr: "64-character hex"},
		{name: "no expiry", mutate: func(rt *RefreshToken) { rt.ExpiresAt = time.Time{} }, wantErr: "expires_at is required"},
		{name: "no activity", mutate: func(rt *RefreshToken) { rt.LastActivity = time.Time{} }, wantErr: "last_activity is required"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt := valid()
			tt.mutate(&rt)
			err := rt.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestRefreshTokenSessionFallbacks(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	started := created.Add(-time.Hour)

	withoutFamily := RefreshToken{ID: "rt-1", CreatedAt: created}
	assert.Equal(t, "rt-1", withoutFamily.SessionFamily(), "a token without a family is its own family")
	assert.Equal(t, created, withoutFamily.SessionStart(), "without a session start the creation time is used")

	inFamily := RefreshToken{ID: "rt-2", FamilyID: "rt-1", CreatedAt: created, SessionStartedAt: started}
	assert.Equal(t, "rt-1", inFamily.SessionFamily())
	assert.Equal(t, started, inFamily.SessionStart())
}

func TestRefreshTokenJSONHidesHash(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(RefreshToken{ID: "rt", TokenHash: testTokenHash})
	require.NoError(t, err)
	assert.NotContains(t, string(b), testTokenHash)
	assert.NotContains(t, string(b), "token_hash")
}

func TestGenerateAPIKey(t *testing.T) {
	t.Parallel()
	raw, prefix, hash, err := GenerateAPIKey()
	require.NoError(t, err)

	assert.Len(t, raw, 64)
	assert.Equal(t, raw[:APIKeyPrefixLen], prefix)
	assert.Len(t, hash, 64)
	assert.NotEqual(t, raw, hash, "the stored value is a hash, not the key")
	assert.True(t, VerifyAPIKeyHash(raw, hash))

	raw2, _, hash2, err := GenerateAPIKey()
	require.NoError(t, err)
	assert.NotEqual(t, raw, raw2)
	assert.NotEqual(t, hash, hash2)
}

func TestVerifyAPIKeyHash(t *testing.T) {
	t.Parallel()
	raw, _, hash, err := GenerateAPIKey()
	require.NoError(t, err)

	tests := []struct {
		name string
		raw  string
		hash string
		want bool
	}{
		{name: "match", raw: raw, hash: hash, want: true},
		{name: "other key", raw: strings.Repeat("0", 64), hash: hash},
		{name: "empty key", raw: "", hash: hash},
		{name: "empty hash", raw: raw, hash: ""},
		{name: "raw key stored as hash", raw: raw, hash: raw},
		{name: "upper-case hash", raw: raw, hash: strings.ToUpper(hash)},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, VerifyAPIKeyHash(tt.raw, tt.hash))
		})
	}
}

func TestAPIKeyValidate(t *testing.T) {
	t.Parallel()
	valid := func() APIKey {
		return APIKey{UserID: "u1", Name: "ci", KeyHash: testTokenHash, Prefix: testTokenHash[:APIKeyPrefixLen]}
	}

	tests := []struct {
		name    string
		mutate  func(*APIKey)
		wantErr string
	}{
		{name: "valid", mutate: func(*APIKey) {}},
		{name: "no user", mutate: func(k *APIKey) { k.UserID = "" }, wantErr: "user_id is required"},
		{name: "no name", mutate: func(k *APIKey) { k.Name = " " }, wantErr: "name is required"},
		{name: "long name", mutate: func(k *APIKey) { k.Name = strings.Repeat("n", MaxAPIKeyNameLen+1) }, wantErr: "name must be at most"},
		{name: "plaintext key as hash", mutate: func(k *APIKey) { k.KeyHash = "raw-key" }, wantErr: "64-character hex"},
		{name: "no prefix", mutate: func(k *APIKey) { k.Prefix = "" }, wantErr: "prefix is required"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			k := valid()
			tt.mutate(&k)
			err := k.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestAPIKeyJSONHidesHash(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(APIKey{ID: "k", KeyHash: testTokenHash, Prefix: "0123456789abcdef"})
	require.NoError(t, err)
	assert.NotContains(t, string(b), testTokenHash)
	assert.NotContains(t, string(b), "key_hash")
	assert.Contains(t, string(b), `"prefix":"0123456789abcdef"`)
}

func TestAuditLogValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		log     AuditLog
		wantErr string
	}{
		{name: "valid", log: AuditLog{UserID: "u1", Action: "create", EntityType: "item", EntityID: "1"}},
		{name: "no user", log: AuditLog{Action: "create", EntityType: "item"}, wantErr: "user_id is required"},
		{name: "no action", log: AuditLog{UserID: "u1", EntityType: "item"}, wantErr: "action is required"},
		{name: "no entity type", log: AuditLog{UserID: "u1", Action: "create"}, wantErr: "entity_type is required"},
		{name: "long entity id", log: AuditLog{UserID: "u1", Action: "create", EntityType: "item", EntityID: strings.Repeat("x", MaxAuditEntityIDLen+1)}, wantErr: "entity_id must be at most"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.log.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
