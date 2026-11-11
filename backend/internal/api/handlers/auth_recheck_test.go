package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"backend/internal/database"
	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecheckUserActive(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("dial tcp")
	tests := []struct {
		findErr error
		mutate  func(r *mockUserRepo)
		want    error
		name    string
	}{
		{name: "active", want: nil},
		{name: "disabled", mutate: func(r *mockUserRepo) { r.update("u1", func(u *models.User) { u.Disabled = true }) }, want: errAccountDisabled},
		{name: "deleted", mutate: func(r *mockUserRepo) { r.remove("u1") }, want: errUserGone},
		{name: "repository error", findErr: dbErr, want: dbErr},
		{name: "cancelled", findErr: context.Canceled, want: context.Canceled},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newMockUserRepo(localUser("u1", "alice"))
			repo.findByIDErr = tt.findErr
			if tt.mutate != nil {
				tt.mutate(repo)
			}
			err := recheckUserActive(context.Background(), repo, "u1")
			if tt.want == nil {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tt.want)
		})
	}
}

func TestRecheckSessionActive(t *testing.T) {
	t.Parallel()
	tokens := database.NewGORMRefreshTokenRepository(openRefreshDB(t))
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, tokens.Create(ctx, &models.RefreshToken{ID: "live", UserID: "u1", TokenHash: sha256Hex("live"), ExpiresAt: now.Add(time.Hour), LastActivity: now}))
	require.NoError(t, tokens.Create(ctx, &models.RefreshToken{ID: "dead", UserID: "u1", TokenHash: sha256Hex("dead"), ExpiresAt: now.Add(time.Hour), LastActivity: now, Revoked: true}))

	require.NoError(t, recheckSessionActive(ctx, tokens, "live"))
	require.ErrorIs(t, recheckSessionActive(ctx, tokens, "dead"), errSessionRevoked)
	require.ErrorIs(t, recheckSessionActive(ctx, tokens, "unknown"), errSessionRevoked)
	dbErr := errors.New("dial tcp")
	require.ErrorIs(t, recheckSessionActive(ctx, &faultyTokens{RefreshTokenRepository: tokens, countFamilyErr: dbErr}, "live"), dbErr)
}
