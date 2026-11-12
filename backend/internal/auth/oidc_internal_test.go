package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// The allow-list never has none or an HMAC algorithm: an HMAC key derived from
// the client secret would let anyone who knows the secret forge tokens.
func TestAllowedSigningAlgs(t *testing.T) {
	t.Parallel()
	require.NotEmpty(t, allowedSigningAlgs)
	for _, alg := range allowedSigningAlgs {
		assert.NotEqual(t, "none", strings.ToLower(alg))
		assert.False(t, strings.HasPrefix(strings.ToUpper(alg), "HS"), alg)
	}
	assert.Contains(t, allowedSigningAlgs, "RS256")
	assert.Contains(t, allowedSigningAlgs, "ES256")
}

func TestSafeCode(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"invalid_grant", "invalid_grant"},
		{"", "unknown"},
		{"has space", "unknown"},
		{"<script>", "unknown"},
		{"line\nbreak", "unknown"},
		{strings.Repeat("a", 65), "unknown"},
		{strings.Repeat("a", 64), strings.Repeat("a", 64)},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, safeCode(tt.in))
		})
	}
}

func TestTransportReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "too large", err: &url.Error{Op: "Get", URL: "https://x/secret?code=1", Err: errResponseTooLarge}, want: "response too large"},
		{name: "deadline", err: &url.Error{Op: "Post", URL: "https://x", Err: context.DeadlineExceeded}, want: "timeout"},
		{name: "cancelled", err: context.Canceled, want: "cancelled"},
		{name: "anything else", err: errors.New("dial tcp 10.0.0.1:443: password=hunter2"), want: "connection or response error"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := transportReason(tt.err)
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, "secret")
			assert.NotContains(t, got, "hunter2")
		})
	}
}

func TestMapRole(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "admin", mapRole([]string{"a"}, []string{"d"}, []string{"d", "a"}))
	assert.Equal(t, "devops", mapRole([]string{"a"}, []string{"d"}, []string{"d"}))
	assert.Equal(t, "user", mapRole([]string{"a"}, []string{"d"}, []string{"A", "D"}))
	assert.Equal(t, "user", mapRole(nil, nil, []string{"a", "admin"}))
	assert.Equal(t, "user", mapRole([]string{""}, []string{""}, []string{""}))
}

func TestCleanText(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "ab", cleanText("a\x00\tb\n", 10))
	assert.Equal(t, "abc", cleanText("  abc  ", 10))
	assert.Equal(t, "éé", cleanText("ééé", 2))
	assert.Empty(t, cleanText("\x00\x1f", 10))
}

func TestClaimStrings(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"a"}, claimStrings(map[string]any{"r": "a"}, "r"))
	assert.Equal(t, []string{"a", "b"}, claimStrings(map[string]any{"r": []any{"a", 1, "b", nil}}, "r"))
	assert.Nil(t, claimStrings(map[string]any{"r": 5.0}, "r"))
	assert.Nil(t, claimStrings(map[string]any{}, "r"))
	many := make([]any, 500)
	for i := range many {
		many[i] = "r"
	}
	assert.Len(t, claimStrings(map[string]any{"r": many}, "r"), maxRoleValues, "the number of roles read is bounded")
}

func TestValidEmail(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "a@example.com", validEmail("  a@example.com "))
	assert.Empty(t, validEmail("not an email"))
	assert.Empty(t, validEmail(""))
	assert.Empty(t, validEmail(strings.Repeat("a", 250)+"@example.com"))
}

func TestAuthStyle(t *testing.T) {
	t.Parallel()
	assert.Equal(t, oauth2.AuthStyleInHeader, authStyle(nil))
	assert.Equal(t, oauth2.AuthStyleInHeader, authStyle([]string{"client_secret_basic", "client_secret_post"}))
	assert.Equal(t, oauth2.AuthStyleInParams, authStyle([]string{"client_secret_post"}))
	assert.Equal(t, oauth2.AuthStyleInHeader, authStyle([]string{"private_key_jwt"}))
}

// The limited body fails when the body is over the limit, also when the
// limit is hit exactly at the end of a read, and passes a body that is exactly
// at the limit.
func TestLimitedBody(t *testing.T) {
	t.Parallel()
	read := func(body string, limit int64) (string, error) {
		b := &limitedBody{rc: io.NopCloser(strings.NewReader(body)), left: limit}
		out, err := io.ReadAll(b)
		return string(out), err
	}
	got, err := read("12345", 5)
	require.NoError(t, err)
	assert.Equal(t, "12345", got, "a body at the limit is fine")
	_, err = read("123456", 5)
	require.ErrorIs(t, err, errResponseTooLarge)
	_, err = read("12345678901234567890", 5)
	require.ErrorIs(t, err, errResponseTooLarge)
	got, err = read("", 5)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestHTTPClientRefusesRedirectsAndLargeBodies(t *testing.T) {
	t.Parallel()
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Store(true) }))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, target.URL, http.StatusFound)
		case "/declared-large":
			w.Header().Set("Content-Length", "2097152")
			_, _ = w.Write([]byte("x"))
		case "/streamed-large":
			chunk := []byte(strings.Repeat("x", 64<<10))
			for i := 0; i < 32; i++ {
				_, _ = w.Write(chunk)
				w.(http.Flusher).Flush()
			}
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	t.Cleanup(srv.Close)
	client := newHTTPClient(2 * time.Second)

	resp, err := client.Get(srv.URL + "/redirect")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusFound, resp.StatusCode, "the redirect is returned, not followed")
	assert.False(t, followed.Load())

	_, err = client.Get(srv.URL + "/declared-large")
	require.ErrorIs(t, err, errResponseTooLarge)

	resp, err = client.Get(srv.URL + "/streamed-large")
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.ErrorIs(t, err, errResponseTooLarge)

	resp, err = client.Get(srv.URL + "/ok")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, "ok", string(body))
}

// The token endpoint answer decides who has to act. A 401, or an invalid_client
// or unauthorized_client code (any status), says this server's client
// credentials are refused: that is a configuration problem and an operator must
// act, so it is "provider unavailable" (the user cannot fix it by trying again)
// and flagged as a configuration problem. Any other 4xx is the provider refusing
// this login (a used or expired code): "rejected". 5xx and transport failures
// are "unavailable" without the flag.
func TestExchangeError(t *testing.T) {
	t.Parallel()
	retrieve := func(status int, code string) error {
		return &oauth2.RetrieveError{
			Response:  &http.Response{StatusCode: status},
			Body:      []byte(`{"error":"` + code + `","error_description":"provider-text"}`),
			ErrorCode: code,
		}
	}
	tests := []struct {
		name       string
		err        error
		wantKind   error
		wantConfig bool
	}{
		{name: "400 invalid_grant", err: retrieve(400, "invalid_grant"), wantKind: ErrLoginRejected},
		{name: "400 without a code", err: retrieve(400, ""), wantKind: ErrLoginRejected},
		{name: "403", err: retrieve(403, "access_denied"), wantKind: ErrLoginRejected},
		{name: "404", err: retrieve(404, ""), wantKind: ErrLoginRejected},
		{name: "429", err: retrieve(429, "slow_down"), wantKind: ErrLoginRejected},
		{name: "401 invalid_client", err: retrieve(401, "invalid_client"), wantKind: ErrProviderUnavailable, wantConfig: true},
		{name: "401 without a code", err: retrieve(401, ""), wantKind: ErrProviderUnavailable, wantConfig: true},
		{name: "401 invalid_grant", err: retrieve(401, "invalid_grant"), wantKind: ErrProviderUnavailable, wantConfig: true},
		{name: "400 invalid_client", err: retrieve(400, "invalid_client"), wantKind: ErrProviderUnavailable, wantConfig: true},
		{name: "400 unauthorized_client", err: retrieve(400, "unauthorized_client"), wantKind: ErrProviderUnavailable, wantConfig: true},
		{name: "403 unauthorized_client", err: retrieve(403, "unauthorized_client"), wantKind: ErrProviderUnavailable, wantConfig: true},
		{name: "500", err: retrieve(500, "server_error"), wantKind: ErrProviderUnavailable},
		{name: "503", err: retrieve(503, ""), wantKind: ErrProviderUnavailable},
		{name: "transport failure", err: errors.New("dial tcp 10.0.0.1:443: connect: refused"), wantKind: ErrProviderUnavailable},
		{name: "RetrieveError without a response", err: &oauth2.RetrieveError{ErrorCode: "invalid_client"}, wantKind: ErrProviderUnavailable},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := exchangeError(tt.err)

			require.ErrorIs(t, err, tt.wantKind)
			var perr *Error
			require.ErrorAs(t, err, &perr)
			assert.Equal(t, tt.wantConfig, perr.Misconfigured)
			// Nothing the provider sent or the transport said is in the error.
			for _, leak := range []string{"provider-text", "10.0.0.1", "dial tcp"} {
				assert.NotContains(t, err.Error(), leak)
			}
		})
	}
}
