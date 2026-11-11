package handlers

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"backend/internal/models"
	"backend/internal/sessionstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flakyBlockStore fails BlockUser by a plan: fail(n) says whether the n-th
// call (from 1, over the lifetime of the store) fails.
type flakyBlockStore struct {
	sessionstore.SessionStore
	fail  func(call int) bool
	mu    sync.Mutex
	calls int
}

func (f *flakyBlockStore) BlockUser(ctx context.Context, userID string, until time.Time) error {
	f.mu.Lock()
	f.calls++
	n, plan := f.calls, f.fail
	f.mu.Unlock()
	if plan != nil && plan(n) {
		return errors.New("dial tcp 10.0.0.1:3306: i/o timeout")
	}
	return f.SessionStore.BlockUser(ctx, userID, until)
}

func (f *flakyBlockStore) setPlan(plan func(call int) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = plan
}

func (f *flakyBlockStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newFlakyEnv(t *testing.T, plan func(call int) bool, extra ...func(*adminOpt)) (*adminEnv, *flakyBlockStore) {
	t.Helper()
	var flaky *flakyBlockStore
	opt := adminOpt{store: func(s sessionstore.SessionStore) sessionstore.SessionStore {
		flaky = &flakyBlockStore{SessionStore: s, fail: plan}
		return flaky
	}}
	for _, f := range extra {
		f(&opt)
	}
	return newAdminEnv(t, opt), flaky
}

func always(int) bool { return true }

func TestUserBlockIsRetriedWithinOneRequest(t *testing.T) {
	t.Parallel()
	e, flaky := newFlakyEnv(t, func(call int) bool { return call == 1 })
	tok, _ := e.login(t, "user-u")

	w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/role", `{"role":"devops"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 2, flaky.callCount(), "one failure, one success")
	assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(tok)), "the block was written")
	assert.Contains(t, e.logs.String(), "Retrying the user block")
}

func TestUserBlockRetriesAreBounded(t *testing.T) {
	t.Parallel()
	e, flaky := newFlakyEnv(t, always)
	w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/disabled", `{"disabled":true}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, 1+len(e.handler.blockRetryDelays), flaky.callCount(), "the first attempt and one per delay")
}

func TestUserBlockRetryStopsWhenTheStepBudgetEnds(t *testing.T) {
	t.Parallel()
	e, flaky := newFlakyEnv(t, always)
	e.handler.blockRetryDelays = []time.Duration{time.Hour, time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := e.handler.blockUser(ctx, idUserU)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second, "the wait ends with the context")
	assert.Equal(t, 1, flaky.callCount())

	// A cancelled context and an invalid key are not retried.
	before := flaky.callCount()
	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	flaky.setPlan(func(int) bool { return false })
	assert.Error(t, e.handler.blockUser(cancelled, idUserU))
	assert.Equal(t, before+1, flaky.callCount(), "one attempt, not repeated")
}

// TestRoleChangeRevocationCanBeFinishedByRetrying: the role change commits,
// the block and the refresh revoke fail, the answer is 500; the same request
// again, with the stores healthy again, revokes and answers 200 without a
// second audit row.
func TestRoleChangeRevocationCanBeFinishedByRetrying(t *testing.T) {
	t.Parallel()
	var tokens *faultyTokens
	e, flaky := newFlakyEnv(t, always, func(o *adminOpt) {
		o.tokens = func(r models.RefreshTokenRepository) models.RefreshTokenRepository {
			tokens = &faultyTokens{RefreshTokenRepository: r, revokeErr: errors.New("Error 1205: lock wait timeout")}
			return tokens
		}
	})
	// Sign in through the real repository so a refresh token exists.
	tokens.revokeErr = nil
	tok, _ := e.login(t, "user-u")
	require.EqualValues(t, 1, e.activeRefreshTokens(t, idUserU))
	tokens.revokeErr = errors.New("Error 1205: lock wait timeout")

	w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/role", `{"role":"devops"}`)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, msgInternalServerError, errorOf(t, w))
	assert.Equal(t, models.RoleDevOps, e.user(t, idUserU).Role, "the change committed")
	assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(tok)), "the old token still works: the block failed")
	assert.EqualValues(t, 1, e.activeRefreshTokens(t, idUserU), "the families were not revoked")
	users, _ := e.revoker.snapshot()
	assert.Equal(t, []string{idUserU}, users, "the sockets are closed even then")

	// The stores recover; the same request finishes the revocation.
	flaky.setPlan(nil)
	tokens.revokeErr = nil
	w = e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/role", `{"role":"devops"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decodeInto[ChangeRoleResponse](t, w)
	assert.False(t, resp.Changed)
	assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(tok)), "the user is blocked now")
	assert.Zero(t, e.activeRefreshTokens(t, idUserU), "the families are revoked")
	users, _ = e.revoker.snapshot()
	assert.Equal(t, []string{idUserU, idUserU}, users, "and the sockets disconnected again")
	tok2, _ := e.login(t, "user-u")
	assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(tok2)), "a login after the retry works")

	rows := e.auditRows(t)
	require.Len(t, rows, 1, "the retry is a no-op and writes no second audit row; the first request was recorded as committed")
	assert.Contains(t, rows[0].Details, `"revocation_failed":true`)
}

func TestNoOpRoleRetryThatFailsAgainIsStill500(t *testing.T) {
	t.Parallel()
	e, _ := newFlakyEnv(t, always)
	w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/role", `{"role":"user"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, msgInternalServerError, errorOf(t, w))
	assert.Empty(t, e.auditRows(t), "a no-op is never audited, also when it fails")
}

func TestDeleteWritesTheBlockFirst(t *testing.T) {
	t.Parallel()

	t.Run("a failing pre-block deletes nothing and a retry after recovery deletes and blocks", func(t *testing.T) {
		t.Parallel()
		e, flaky := newFlakyEnv(t, always)
		tok, _ := e.login(t, "user-u")
		raw, _ := e.newKey(t, idUserU)

		w := e.asAdmin(t, http.MethodDelete, "/api/v1/users/"+idUserU, "")
		require.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, msgInternalServerError, errorOf(t, w))
		assert.NotContains(t, w.Body.String(), "dial tcp")
		assert.True(t, e.userExists(idUserU), "nothing was deleted")
		assert.EqualValues(t, 1, e.keyCount(t, idUserU))
		assert.EqualValues(t, 1, e.activeRefreshTokens(t, idUserU))
		assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(tok)))
		assert.Equal(t, http.StatusOK, e.tokenStatus(keyHeader(raw)))
		users, _ := e.revoker.snapshot()
		assert.Empty(t, users)
		assert.Equal(t, 1+len(e.handler.blockRetryDelays), flaky.callCount())

		flaky.setPlan(nil)
		w = e.asAdmin(t, http.MethodDelete, "/api/v1/users/"+idUserU, "")
		require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
		assert.False(t, e.userExists(idUserU))
		assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(tok)), "blocked")
		assert.Zero(t, e.keyCount(t, idUserU))
		assert.Zero(t, e.activeRefreshTokens(t, idUserU))
		users, _ = e.revoker.snapshot()
		assert.Equal(t, []string{idUserU}, users)
		rows := e.auditRows(t)
		require.Len(t, rows, 1, "only the delete that happened")
		assert.Equal(t, "delete", rows[0].Action)
	})

	t.Run("a failing post-block still succeeds: the pre-delete block is in place", func(t *testing.T) {
		t.Parallel()
		// The first call (before the delete) passes, every later one fails.
		e, flaky := newFlakyEnv(t, func(call int) bool { return call > 1 })
		tok, _ := e.login(t, "user-u")
		raw, _ := e.newKey(t, idUserU)

		w := e.asAdmin(t, http.MethodDelete, "/api/v1/users/"+idUserU, "")
		require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
		assert.False(t, e.userExists(idUserU))
		assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(tok)), "blocked by the write before the delete")
		assert.Zero(t, e.keyCount(t, idUserU))
		assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(keyHeader(raw)))
		assert.Zero(t, e.activeRefreshTokens(t, idUserU))
		users, _ := e.revoker.snapshot()
		assert.Equal(t, []string{idUserU}, users)
		assert.Equal(t, 2+len(e.handler.blockRetryDelays), flaky.callCount(), "one pre-block, then the post-block with its retries")
		assert.Contains(t, e.logs.String(), "Failed to block the user")
	})

	t.Run("a post-delete step other than the block that fails is still 500", func(t *testing.T) {
		t.Parallel()
		e, _ := newFlakyEnv(t, nil, func(o *adminOpt) {
			o.keys = func(r models.APIKeyRepository) models.APIKeyRepository { return failingDeleteAll{APIKeyRepository: r} }
		})
		w := e.asAdmin(t, http.MethodDelete, "/api/v1/users/"+idUserU, "")
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.False(t, e.userExists(idUserU))
	})

	t.Run("without a session store the delete still works", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		e.handler.SetSessionStore(nil)
		w := e.asAdmin(t, http.MethodDelete, "/api/v1/users/"+idUserU, "")
		assert.Equal(t, http.StatusNoContent, w.Code)
	})
}
