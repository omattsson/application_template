package handlers

import (
	"errors"
	"net/http"
	"testing"

	"backend/internal/api/middleware"

	"github.com/stretchr/testify/assert"
)

// bindAuthMetrics installs an in-memory MeterProvider and binds the auth
// counters to it. Not for parallel tests: both are process-wide.
func bindAuthMetrics(t *testing.T) func(name, attr string) map[string]int64 {
	t.Helper()
	reader := installMeterReader(t)
	middleware.RebindAuthMeter()
	t.Cleanup(middleware.RebindAuthMeter) // runs before the provider is reset (LIFO)
	return func(name, attr string) map[string]int64 { return counterTotals(t, reader, name, attr) }
}

// TestRefreshSigningFailureIsCountedAsError: the 500 of a refresh whose new
// access token cannot be signed is an "error" outcome, not a gap in the metric.
func TestRefreshSigningFailureIsCountedAsError(t *testing.T) {
	s := newSessionHarness(t, nil)
	_, raw := s.login(t, "alice")
	counts := bindAuthMetrics(t)
	s.h.signToken = func(middleware.GenerateTokenOptions) (string, error) { return "", errors.New("boom") }

	w, _, _ := s.refresh(t, raw)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.JSONEq(t, `{"error":"Internal server error"}`, w.Body.String())
	assert.Equal(t, map[string]int64{"error": 1}, counts("auth.token.refresh.total", "status"))
}

// TestLoginFailureReasonLabelIsBounded: a reason outside the fixed set (a
// future caller passing free text) becomes "other", never a new series.
func TestLoginFailureReasonLabelIsBounded(t *testing.T) {
	s := newSessionHarness(t, nil)
	counts := bindAuthMetrics(t)

	s.h.logLoginFailure(loginFailure{reason: loginReasonPasswordMismatch, httpCode: http.StatusUnauthorized})
	s.h.logLoginFailure(loginFailure{reason: "free text from a user: alice", httpCode: http.StatusUnauthorized})
	s.h.logLoginFailure(loginFailure{reason: "another free text", httpCode: http.StatusUnauthorized})

	assert.Equal(t, map[string]int64{loginReasonPasswordMismatch: 1, "other": 2}, counts("auth.login.total", "reason"))
}
