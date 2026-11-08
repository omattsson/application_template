package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// wsContextKey is the gin.Context key under which RedactWSToken passes the
// WebSocket authentication token to the handler. Read it with
// WSTokenFromContext so the contract stays in one place.
const wsContextKey = "ws.token"

// RedactWSToken strips the `token` query parameter from `/ws` requests
// before any logging or tracing middleware can capture it.
//
// The WebSocket endpoint accepts a token in a `?token=<jwt>` query parameter
// for browsers that cannot send an Authorization header on an upgrade.
// Middleware that records the request URL (access logs, tracing such as
// otelgin's `http.target`, metrics) would otherwise persist the token. This
// middleware moves the value into the gin.Context and rewrites the URL, so
// the rest of the chain only sees the path and the other query parameters.
//
// Register it before any middleware that reads `c.Request.URL.RawQuery` or
// `c.Request.RequestURI`. It does nothing for paths other than `/ws` and
// `/ws/...`. The handler reads the token with WSTokenFromContext.
func RedactWSToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Match `/ws` exactly or any descendant (`/ws/v2`). `/wsfoo` and
		// `/wso` are not matched: the next segment must be path-separated.
		path := c.Request.URL.Path
		if path != "/ws" && !strings.HasPrefix(path, "/ws/") {
			c.Next()
			return
		}

		q := c.Request.URL.Query()
		// Test for the key, not the value: `/ws?token=` is scrubbed like
		// `/ws?token=<jwt>`, so the parameter name does not reach the logs.
		if _, hasToken := q["token"]; !hasToken {
			c.Next()
			return
		}

		// Stash a non-empty value for the handler. An empty `?token=` is
		// treated as "no credential".
		if token := q.Get("token"); token != "" {
			c.Set(wsContextKey, token)
		}

		q.Del("token")
		c.Request.URL.RawQuery = q.Encode()
		if c.Request.RequestURI != "" {
			// Keep the request struct consistent for code that reads RequestURI.
			c.Request.RequestURI = c.Request.URL.RequestURI()
		}
		c.Next()
	}
}

// WSTokenFromContext returns the token that RedactWSToken extracted from the
// query string. An empty string means the request carried no token; the
// handler should then fall back to the Authorization header before failing
// the upgrade.
func WSTokenFromContext(c *gin.Context) string {
	if v, ok := c.Get(wsContextKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}

	return ""
}
