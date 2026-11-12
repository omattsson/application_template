package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"backend/internal/sessionstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockUserAndWait writes a user block as logout-all does (the block
// millisecond is over when it returns), without revoking any refresh token:
// the case where RevokeAllForUser failed or lost a race.
func blockUserAndWait(t *testing.T, s *sessionHarness, userID string) {
	t.Helper()
	require.NoError(t, blockUserNow(context.Background(), s.store, userID, time.Now().Add(time.Hour)))
}

// A user block ends every session that started before it, even when its
// refresh token is still active (the revoke failed): the refresh answers the
// generic 401, revokes the family and deletes the cookie.
func TestRefreshRefusesASessionThatStartedBeforeAUserBlock(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	_, raw := s.login(t, "alice")
	family := s.row(t, raw).FamilyID
	blockUserAndWait(t, s, "u1")
	require.Equal(t, int64(1), s.familyActive(t, family), "the token is still active")

	w, _, _ := s.refresh(t, raw)

	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.JSONEq(t, invalidSessionBody, w.Body.String(), "the generic answer")
	assertCookieCleared(t, w)
	assert.Zero(t, s.familyActive(t, family), "the session is revoked")
	assert.Contains(t, s.logs.String(), refuseSessionRevoked)
}

// A session that started after the block (and its millisecond) is not caught.
func TestRefreshAllowsASessionThatStartedAfterAUserBlock(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	blockUserAndWait(t, s, "u1")
	_, raw := s.login(t, "alice")

	w, token, next := s.refresh(t, raw)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotEmpty(t, token)
	assert.NotEmpty(t, next)
	assert.Equal(t, http.StatusOK, s.me(t, token), "its access token works too")
}

// The block is per user.
func TestRefreshUserBlockIsPerUser(t *testing.T) {
	t.Parallel()
	s := newSessionHarness(t, nil)
	_, rawBob := s.login(t, "bob")
	blockUserAndWait(t, s, "u1")

	w, _, _ := s.refresh(t, rawBob)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// A failing or cancelled block lookup fails closed: 500 (499 for a cancelled
// request), nothing is revoked and no token is issued.
func TestRefreshUserBlockLookupFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err      error
		name     string
		wantBody string
		wantCode int
	}{
		{name: "store error", err: errors.New("dial tcp: secret detail"), wantCode: http.StatusInternalServerError, wantBody: `{"error":"Internal server error"}`},
		{name: "deadline", err: context.DeadlineExceeded, wantCode: http.StatusInternalServerError, wantBody: `{"error":"Internal server error"}`},
		{name: "cancelled", err: context.Canceled, wantCode: statusClientClosedRequest, wantBody: `{"error":"Client closed request"}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newSessionHarness(t, nil)
			_, raw := s.login(t, "alice")
			s.h.SetSessionStore(&faultyStore{SessionStore: s.store, userErr: tt.err})

			w, _, _ := s.refresh(t, raw)

			assert.Equal(t, tt.wantCode, w.Code, w.Body.String())
			assert.JSONEq(t, tt.wantBody, w.Body.String())
			assert.NotContains(t, w.Body.String(), "secret detail")
			assert.False(t, s.row(t, raw).Revoked, "a failed read changes nothing")
		})
	}
}

// The grace path checks the user block before it spends the grace: a user
// block makes the late tab a replay (401, session revoked), and a failing
// lookup is a 500 that spends nothing.
func TestRefreshGraceChecksTheUserBlock(t *testing.T) {
	t.Parallel()

	t.Run("blocked user: replay", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		_, raw := s.login(t, "alice")
		w, _, next := s.refresh(t, raw)
		require.Equal(t, http.StatusOK, w.Code)
		family := s.row(t, next).FamilyID
		blockUserAndWait(t, s, "u1")

		w, token, _ := s.refresh(t, raw)

		assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		assert.JSONEq(t, invalidSessionBody, w.Body.String())
		assert.Empty(t, token)
		assert.Zero(t, s.familyActive(t, family))
	})

	t.Run("store error: 500 and the grace is kept", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		_, raw := s.login(t, "alice")
		w, _, next := s.refresh(t, raw)
		require.Equal(t, http.StatusOK, w.Code)
		s.h.SetSessionStore(&faultyStore{SessionStore: s.store, userErr: errors.New("dial tcp")})

		w, _, _ = s.refresh(t, raw)

		assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		assert.JSONEq(t, `{"error":"Internal server error"}`, w.Body.String())
		s.h.SetSessionStore(s.store)
		assert.Equal(t, int64(1), s.familyActive(t, s.row(t, next).FamilyID), "nothing revoked")
		w, _, _ = s.refresh(t, raw)
		assert.Equal(t, http.StatusOK, w.Code, "the grace was not spent by the failed request")
	})

	t.Run("session after the block: grace works", func(t *testing.T) {
		t.Parallel()
		s := newSessionHarness(t, nil)
		blockUserAndWait(t, s, "u1")
		_, raw := s.login(t, "alice")
		w, _, _ := s.refresh(t, raw)
		require.Equal(t, http.StatusOK, w.Code)

		w, token, _ := s.refresh(t, raw)

		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.NotEmpty(t, token)
	})
}

var _ sessionstore.SessionStore = (*faultyStore)(nil)
