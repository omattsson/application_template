package handlers

import (
	"net/http"
	"testing"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRefreshCookieNameAndAttributes checks the refresh cookie in both
// SECURE_COOKIES modes: the name (__Host- only with Secure), Path=/, no
// Domain, identical attributes on set and clear, and that Refresh and Logout
// read the cookie by that name (and not by the other one).
func TestRefreshCookieNameAndAttributes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		secure    bool
		wantName  string
		otherName string
	}{
		{name: "secure", secure: true, wantName: "__Host-refresh_token", otherName: "refresh_token"},
		{name: "plain http development", secure: false, wantName: "refresh_token", otherName: "__Host-refresh_token"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newSessionHarness(t, func(c *config.AuthConfig) { c.SecureCookies = tt.secure })
			named := func(name, raw string) reqOpt {
				return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: name, Value: raw}) }
			}

			w := s.post(t, "/api/v1/auth/login", loginBody(t, "alice", testPassword))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var set *http.Cookie
			for _, ck := range w.Result().Cookies() {
				require.Equal(t, tt.wantName, ck.Name, "the only cookie of a login")
				set = ck
			}
			require.NotNil(t, set)
			assert.Equal(t, "/", set.Path)
			assert.Equal(t, tt.secure, set.Secure)
			assert.True(t, set.HttpOnly)
			assert.Empty(t, set.Domain)
			assert.Equal(t, http.SameSiteStrictMode, set.SameSite)
			raw := set.Value

			// The cookie under the other name is not read.
			wOther := s.post(t, "/api/v1/auth/refresh", "", named(tt.otherName, raw))
			assert.Equal(t, http.StatusUnauthorized, wOther.Code)

			// Refresh reads it by its name and sets the next one the same way.
			wr := s.post(t, "/api/v1/auth/refresh", "", named(tt.wantName, raw))
			require.Equal(t, http.StatusOK, wr.Code, wr.Body.String())
			next := wr.Result().Cookies()
			require.Len(t, next, 1)
			assert.Equal(t, tt.wantName, next[0].Name)
			assert.Equal(t, set.Path, next[0].Path)
			assert.Equal(t, set.Secure, next[0].Secure)
			assert.Equal(t, set.SameSite, next[0].SameSite)
			assert.Equal(t, set.Domain, next[0].Domain)

			// Logout reads it by its name and clears it with the same attributes.
			wl := s.post(t, "/api/v1/auth/logout", "", named(tt.wantName, next[0].Value))
			require.Less(t, wl.Code, 300, wl.Body.String())
			cleared := wl.Result().Cookies()
			require.Len(t, cleared, 1)
			assert.Equal(t, tt.wantName, cleared[0].Name)
			assert.Empty(t, cleared[0].Value)
			assert.Equal(t, -1, cleared[0].MaxAge)
			assert.Equal(t, set.Path, cleared[0].Path)
			assert.Equal(t, set.Secure, cleared[0].Secure)
			assert.Equal(t, set.HttpOnly, cleared[0].HttpOnly)
			assert.Equal(t, set.SameSite, cleared[0].SameSite)
			assert.Equal(t, set.Domain, cleared[0].Domain)

			// The logout revoked the session: the last token no longer refreshes.
			wAfter := s.post(t, "/api/v1/auth/refresh", "", named(tt.wantName, next[0].Value))
			assert.Equal(t, http.StatusUnauthorized, wAfter.Code)
		})
	}
}
