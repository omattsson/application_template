package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// useAuthMetrics binds the auth instruments to a fresh in-memory provider and
// restores them afterwards. Not for parallel tests: the instruments are
// process-wide.
func useAuthMetrics(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	RebindAuthMeter()
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(prev)
		RebindAuthMeter()
	})
	return reader
}

// counts returns the value of every series of the counter, keyed by the value
// of the attribute "status" (plus "/reason" or "method/" when present).
func counts(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	c, ok := findCounter(rm, name)
	if !ok {
		return out
	}
	for _, dp := range c.DataPoints {
		status, _ := dp.Attributes.Value("status")
		key := status.AsString()
		if m, ok := dp.Attributes.Value("method"); ok {
			key = m.AsString() + "/" + key
		}
		if r, ok := dp.Attributes.Value("reason"); ok {
			key += "/" + r.AsString()
		}
		out[key] += dp.Value
	}
	return out
}

func TestAuthMetricsRecorders(t *testing.T) {
	reader := useAuthMetrics(t)

	RecordLogin("local", "success")
	RecordLogin("local", "success")
	RecordLoginFailure("local", "failure", "password_mismatch")
	RecordLogin("", "success") // ignored: no method
	RecordLogin("oidc", "")    // ignored: no status
	RecordRefresh("success")
	RecordRefresh("")
	RecordAPIKeyAuth("success")
	RecordAPIKeyAuth("a-status-that-is-not-in-the-fixed-set")
	RecordJWTAuth("revoked")

	assert.Equal(t, map[string]int64{"local/success": 2, "local/failure/password_mismatch": 1}, counts(t, reader, "auth.login.total"))
	assert.Equal(t, map[string]int64{"success": 1}, counts(t, reader, "auth.token.refresh.total"))
	assert.Equal(t, map[string]int64{"success": 1, "other": 1}, counts(t, reader, "auth.apikey.total"))
	assert.Equal(t, map[string]int64{"revoked": 1}, counts(t, reader, "auth.jwt.total"))
}

// TestAuthMetricsFollowTheGlobalProviderAfterRebind: instruments created at
// package initialisation (before telemetry.Init) only take effect after
// RebindAuthMeter.
func TestAuthMetricsFollowTheGlobalProviderAfterRebind(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(prev)
		RebindAuthMeter()
	})

	otel.SetMeterProvider(noopmetric.NewMeterProvider())
	RebindAuthMeter()
	RecordAPIKeyAuth("success") // goes nowhere

	otel.SetMeterProvider(mp)
	RebindAuthMeter()
	RecordAPIKeyAuth("success")

	assert.Equal(t, map[string]int64{"success": 1}, counts(t, reader, "auth.apikey.total"))
}

// TestAuthMetricRecordingAllocatesNothingWhenDisabled: with the no-op
// provider (OTEL_ENABLED=false), recording an outcome on the authenticated
// request path allocates nothing.
func TestAuthMetricRecordingAllocatesNothingWhenDisabled(t *testing.T) {
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(noopmetric.NewMeterProvider())
	RebindAuthMeter()
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		RebindAuthMeter()
	})

	allocs := testing.AllocsPerRun(200, func() {
		RecordAPIKeyAuth(apiKeyOutcomeSuccess)
		RecordJWTAuth(jwtOutcomeSuccess)
	})
	assert.Zero(t, allocs)
}

func TestCombinedAuthRecordsAPIKeyOutcomes(t *testing.T) {
	client, rec := issuedKey(t, "k1", "u1", inHours(24))
	expiredClient, expiredRec := issuedKey(t, "k2", "u1", func() *time.Time { x := time.Now().Add(-time.Second); return &x }())
	disabledClient, disabledRec := issuedKey(t, "k3", "u2", inHours(24))
	users := newUserRepoFake(
		&models.User{ID: "u1", Username: "alice", Role: models.RoleUser},
		&models.User{ID: "u2", Username: "bob", Role: models.RoleUser, Disabled: true},
	)
	keys := newKeyRepoFake(rec, expiredRec, disabledRec)
	unknown := "sk_" + strings.Repeat("e", 64)

	tests := []struct {
		name   string
		key    string
		deps   func(*keyRepoFake, *userRepoFake) APIKeyAuthDeps
		ctx    context.Context
		want   string
		status int
	}{
		{name: "success", key: client, want: "success", status: http.StatusOK},
		{name: "malformed", key: "not-a-key", want: "malformed", status: http.StatusUnauthorized},
		{name: "unknown", key: unknown, want: "unknown", status: http.StatusUnauthorized},
		{name: "expired", key: expiredClient, want: "expired", status: http.StatusUnauthorized},
		{name: "owner disabled", key: disabledClient, want: "owner_unusable", status: http.StatusUnauthorized},
		{
			name: "store error", key: client, want: "error", status: http.StatusInternalServerError,
			deps: func(k *keyRepoFake, u *userRepoFake) APIKeyAuthDeps {
				k.prefixErr = dberrors.NewDatabaseError("find", errors.New("boom"))
				d := keyDeps(k, u)
				d.FailClosedOnStoreError = true
				return d
			},
		},
		{
			name: "cancelled", key: client, want: "cancelled", status: 499,
			deps: func(k *keyRepoFake, u *userRepoFake) APIKeyAuthDeps {
				k.prefixErr = context.Canceled
				return keyDeps(k, u)
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			reader := useAuthMetrics(t)
			k := keys
			if tt.deps != nil {
				k = newKeyRepoFake(rec)
			}
			deps := keyDeps(k, users)
			if tt.deps != nil {
				deps = tt.deps(k, users)
			}
			w := doGet(keyRouter(deps), "/protected", map[string]string{"X-API-Key": tt.key})
			assert.Equal(t, tt.status, w.Code, "the answer does not change with the metric: %s", w.Body.String())
			assert.Equal(t, map[string]int64{tt.want: 1}, counts(t, reader, "auth.apikey.total"))
		})
	}
}

func TestAuthRequiredRecordsJWTOutcomes(t *testing.T) {
	memory := sessionstore.NewMemory()
	t.Cleanup(memory.Stop)
	require.NoError(t, memory.BlockToken(context.Background(), "revoked-jti", time.Now().Add(time.Hour)))
	dbDown := errors.New("db down")

	revoked := validClaims()
	revoked.ID = "revoked-jti"

	tests := []struct {
		store   sessionstore.SessionStore
		headers func(t *testing.T) map[string]string
		name    string
		want    string
		status  int
		strict  bool
	}{
		{name: "success", store: memory, headers: func(t *testing.T) map[string]string { return bearer(t, validClaims()) }, want: "success", status: 200},
		{name: "missing header", store: memory, headers: func(*testing.T) map[string]string { return nil }, want: "missing", status: 401},
		{name: "malformed header", store: memory, headers: func(*testing.T) map[string]string { return map[string]string{"Authorization": "Token x"} }, want: "malformed", status: 401},
		{name: "invalid token", store: memory, headers: func(*testing.T) map[string]string { return map[string]string{"Authorization": "Bearer nope"} }, want: "invalid", status: 401},
		{name: "revoked", store: memory, headers: func(t *testing.T) map[string]string { return bearer(t, revoked) }, want: "revoked", status: 401},
		{
			name: "store error, fail closed", store: failingStore{memory, dbDown}, strict: true, want: "error", status: 500,
			headers: func(t *testing.T) map[string]string { return bearer(t, validClaims()) },
		},
		{
			// Fail open lets the request through: that is a success for the
			// caller, and the failed lookup is the "store_error" outcome of
			// the same request (it is the signal that revocation is not
			// being checked).
			name: "store error, fail open", store: failingStore{memory, dbDown}, want: "success_unchecked", status: 200,
			headers: func(t *testing.T) map[string]string { return bearer(t, validClaims()) },
		},
		{
			name: "cancelled", store: failingStore{memory, context.Canceled}, want: "cancelled", status: 499,
			headers: func(t *testing.T) map[string]string { return bearer(t, validClaims()) },
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			reader := useAuthMetrics(t)
			r := jwtTestRouter(JWTAuthOptions{JWTSecret: testSecret, SessionStore: tt.store, FailClosedOnStoreError: tt.strict})
			w := doGet(r, "/protected", tt.headers(t))
			assert.Equal(t, tt.status, w.Code, w.Body.String())
			assert.Equal(t, map[string]int64{tt.want: 1}, counts(t, reader, "auth.jwt.total"))
		})
	}
}

// TestCombinedAuthRecordsRefusalsBeforeEitherCheck: the requests that
// CombinedAuth refuses before it runs a credential check are counted too.
func TestCombinedAuthRecordsRefusalsBeforeEitherCheck(t *testing.T) {
	client, rec := issuedKey(t, "k1", "u1", inHours(24))
	users := newUserRepoFake(&models.User{ID: "u1", Username: "alice", Role: models.RoleUser})
	r := keyRouter(keyDeps(newKeyRepoFake(rec), users))

	reader := useAuthMetrics(t)
	assert.Equal(t, http.StatusUnauthorized, doGet(r, "/protected", nil).Code)
	assert.Equal(t, http.StatusBadRequest, doGet(r, "/protected", map[string]string{
		"Authorization": "Bearer x", "X-API-Key": client}).Code)
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Add("X-API-Key", client)
	req.Header.Add("X-API-Key", client)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	assert.Equal(t, map[string]int64{"missing": 1}, counts(t, reader, "auth.jwt.total"))
	assert.Equal(t, map[string]int64{"ambiguous": 1, "repeated": 1}, counts(t, reader, "auth.apikey.total"))
}

// TestCombinedAuthCountsANonBearerAuthorizationAsMalformed: "Basic x" without
// a key is not "no credential": the client sent an Authorization header that
// is not a bearer token.
func TestCombinedAuthCountsANonBearerAuthorizationAsMalformed(t *testing.T) {
	_, rec := issuedKey(t, "k1", "u1", inHours(24))
	users := newUserRepoFake(&models.User{ID: "u1", Username: "alice", Role: models.RoleUser})
	r := keyRouter(keyDeps(newKeyRepoFake(rec), users))

	reader := useAuthMetrics(t)
	assert.Equal(t, http.StatusUnauthorized, doGet(r, "/protected", map[string]string{"Authorization": "Basic dXNlcjpwdw=="}).Code)
	assert.Equal(t, http.StatusUnauthorized, doGet(r, "/protected", nil).Code)

	assert.Equal(t, map[string]int64{"malformed": 1, "missing": 1}, counts(t, reader, "auth.jwt.total"))
}
