package middleware

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
)

// Outcomes of the JWT middleware (auth.jwt.total, attribute "status").
const (
	jwtOutcomeSuccess = "success"
	// jwtOutcomeSuccessUnchecked: the request was let through because the
	// session store lookup failed and the middleware fails open. It is the
	// signal that revocation is not being checked.
	jwtOutcomeSuccessUnchecked = "success_unchecked"
	jwtOutcomeMissing          = "missing"   // no credential at all
	jwtOutcomeMalformed        = "malformed" // not "Bearer <token>"
	jwtOutcomeInvalid          = "invalid"   // bad signature, expired, wrong claims
	jwtOutcomeRevoked          = "revoked"   // a blocklist covers the token
	jwtOutcomeError            = "error"     // lookup failed, fail closed (500)
	jwtOutcomeCancelled        = "cancelled" // the client left (499)
)

// Outcomes of the API key authenticator (auth.apikey.total, attribute
// "status"). Only the metric tells them apart: every refusal gives the client
// the same 401.
const (
	apiKeyOutcomeSuccess       = "success"
	apiKeyOutcomeMalformed     = "malformed"      // not sk_ and 64 hex characters
	apiKeyOutcomeUnknown       = "unknown"        // no stored key matches
	apiKeyOutcomeExpired       = "expired"        // the key is past its expiry
	apiKeyOutcomeOwnerUnusable = "owner_unusable" // #nosec G101 -- an outcome label, not a credential; owner deleted, disabled or without a valid role
	apiKeyOutcomeError         = "error"          // lookup failed
	apiKeyOutcomeCancelled     = "cancelled"      // the client left (499)
	apiKeyOutcomeAmbiguous     = "ambiguous"      // a bearer token and a key in one request (400)
	apiKeyOutcomeRepeated      = "repeated"       // the X-API-Key header more than once (401)
)

// authInstruments are the counters of the auth module.
type authInstruments struct {
	login   metric.Int64Counter
	refresh metric.Int64Counter
	apiKey  metric.Int64Counter
	jwt     metric.Int64Counter
	// off is true when the provider is the no-op one: the recorders then
	// return at once. Calling a counter through the metric.Int64Counter
	// interface allocates the option slice even for a no-op counter, which
	// the authenticated request path must not do with telemetry off.
	off bool
}

// authMetrics holds the current instruments. The package creates them at
// initialisation against the global MeterProvider of that moment (the global
// default delegates to the provider that telemetry.Init installs later);
// RebindAuthMeter creates them again.
var authMetrics atomic.Pointer[authInstruments]

func init() {
	RebindAuthMeter()
}

// RebindAuthMeter binds the auth counters to the current global MeterProvider.
// The counters are created when the package is initialised, before
// telemetry.Init runs, so routes.Setup calls it once Init has installed its
// providers (with telemetry off it binds the no-op provider, and recording an
// outcome then costs a load and a branch). Tests that install their own
// provider call it too.
func RebindAuthMeter() {
	_, off := otel.GetMeterProvider().(noopmetric.MeterProvider)
	meter := otel.Meter("auth")
	in := &authInstruments{off: off}

	newCounter := func(dst *metric.Int64Counter, name, desc, unit string) {
		c, err := meter.Int64Counter(name, metric.WithDescription(desc), metric.WithUnit(unit))
		if err != nil {
			otel.Handle(err)
			return
		}
		*dst = c
	}
	newCounter(&in.login, "auth.login.total", "Total login attempts by method and outcome.", "{login}")
	newCounter(&in.refresh, "auth.token.refresh.total", "Total refresh-token attempts by outcome.", "{refresh}")
	newCounter(&in.apiKey, "auth.apikey.total", "Total API-key authentication attempts by outcome.", "{attempt}")
	newCounter(&in.jwt, "auth.jwt.total", "Total bearer-token authentication attempts by outcome.", "{attempt}")

	authMetrics.Store(in)
}

// statusOptions are the attribute sets of the known outcomes, built once: the
// authenticated request path records an outcome without allocating, also with
// telemetry off.
func statusOptions(statuses ...string) map[string]metric.AddOption {
	m := make(map[string]metric.AddOption, len(statuses))
	for _, s := range statuses {
		m[s] = metric.WithAttributeSet(attribute.NewSet(attribute.String("status", s)))
	}
	return m
}

var (
	jwtOptions = statusOptions(jwtOutcomeSuccess, jwtOutcomeSuccessUnchecked, jwtOutcomeMissing,
		jwtOutcomeMalformed, jwtOutcomeInvalid, jwtOutcomeRevoked, jwtOutcomeError, jwtOutcomeCancelled)
	apiKeyOptions = statusOptions(apiKeyOutcomeSuccess, apiKeyOutcomeMalformed, apiKeyOutcomeUnknown,
		apiKeyOutcomeExpired, apiKeyOutcomeOwnerUnusable, apiKeyOutcomeError, apiKeyOutcomeCancelled,
		apiKeyOutcomeAmbiguous, apiKeyOutcomeRepeated)
	refreshOptions = statusOptions("success", "refused", "error")
)

// add counts one event of c under the status, from the table of known
// statuses when it has one.
func add(in *authInstruments, c metric.Int64Counter, known map[string]metric.AddOption, status string) {
	if in.off || c == nil || status == "" {
		return
	}
	opt, ok := known[status]
	if !ok {
		opt = metric.WithAttributes(attribute.String("status", status))
	}
	c.Add(context.Background(), 1, opt)
}

// RecordJWTAuth counts one bearer-token authentication outcome.
func RecordJWTAuth(status string) {
	in := authMetrics.Load()
	add(in, in.jwt, jwtOptions, status)
}

// RecordAPIKeyAuth counts one API key authentication outcome.
func RecordAPIKeyAuth(status string) {
	in := authMetrics.Load()
	add(in, in.apiKey, apiKeyOptions, status)
}

// RecordRefresh counts one refresh-token attempt.
func RecordRefresh(status string) {
	in := authMetrics.Load()
	add(in, in.refresh, refreshOptions, status)
}

// RecordLogin counts one login attempt by method ("local", "oidc") and
// status ("success", "failure").
func RecordLogin(method, status string) {
	RecordLoginFailure(method, status, "")
}

// RecordLoginFailure counts a login attempt like RecordLogin and adds the
// attribute "reason" when reason is not empty. Callers pass only reasons from
// a fixed set of constants (for example the local login reasons in
// handlers/login_failure.go), never user input, so the cardinality stays low.
func RecordLoginFailure(method, status, reason string) {
	in := authMetrics.Load()
	c := in.login
	if in.off || c == nil || method == "" || status == "" {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("method", method),
		attribute.String("status", status),
	}
	if reason != "" {
		attrs = append(attrs, attribute.String("reason", reason))
	}
	c.Add(context.Background(), 1, metric.WithAttributes(attrs...))
}
