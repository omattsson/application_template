package middleware

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// CORSOption changes what CORS allows. Without options the lists are the
// default ones.
type CORSOption func(*corsOptions)

type corsOptions struct {
	idempotency bool
}

// WithIdempotencyHeaders allows the Idempotency-Key request header and
// exposes Idempotency-Replayed and Retry-After to browser scripts. routes.Setup
// passes it with IDEMPOTENCY_ENABLED=true only, so with the module off the
// response headers are unchanged.
func WithIdempotencyHeaders() CORSOption {
	return func(o *corsOptions) { o.idempotency = true }
}

// CORS middleware with configurable allowed origins.
// Pass "*" or "" to allow all origins (development only).
// For production, pass a comma-separated list of allowed origins.
//
// Security invariant (do not change without reading this): the CSRF check of
// the cookie endpoints (requireSameSiteRequest in the handlers package) relies
// on the X-Requested-With header that a cross-origin page cannot send without a
// successful CORS preflight. So the Access-Control-Allow-Headers list below
// must never contain X-Requested-With, and this middleware must never send
// Access-Control-Allow-Credentials (without it a browser does not send or
// accept cookies on a cross-origin fetch at all). TestCORSNeverEnablesCookieCSRFHeader
// fails when either one changes.
func CORS(allowedOrigins string, opts ...CORSOption) gin.HandlerFunc {
	var o corsOptions
	for _, opt := range opts {
		opt(&o)
	}
	allowHeaders := "Content-Type, Content-Length, Accept-Encoding, Authorization, X-Request-ID"
	if o.idempotency {
		allowHeaders += ", " + IdempotencyKeyHeader
	}

	return func(c *gin.Context) {
		if allowedOrigins == "" || allowedOrigins == "*" {
			c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		} else {
			requestOrigin := c.Request.Header.Get("Origin")
			if requestOrigin != "" {
				allowed := false
				for _, origin := range strings.Split(allowedOrigins, ",") {
					if strings.TrimSpace(origin) == requestOrigin {
						c.Writer.Header().Set("Access-Control-Allow-Origin", requestOrigin)
						c.Writer.Header().Set("Vary", "Origin")
						allowed = true
						break
					}
				}
				if !allowed {
					// Block requests from non-whitelisted origins as defense-in-depth;
					// browsers enforce CORS client-side, but we also enforce server-side.
					c.AbortWithStatus(http.StatusForbidden)
					return
				}
			}
			// If there is no Origin header, treat this as a non-CORS request:
			// allow it through without setting Access-Control-Allow-Origin.
		}
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", allowHeaders)
		if o.idempotency {
			// Without it a browser script cannot read these two response headers.
			c.Writer.Header().Set("Access-Control-Expose-Headers", IdempotencyReplayedHeader+", Retry-After")
		}

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// Logger is a middleware that logs incoming requests using structured logging.
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		slog.Info("incoming request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
		)
		c.Next()
	}
}

// msgInternalServerError is the body of a 500. It is the same text as the
// handlers package uses (handlers.msgInternalServerError); middleware does not
// import handlers.
const msgInternalServerError = "Internal server error"

// Recovery is a middleware that recovers from any panics and writes a 500 if there was one.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("recovered from panic", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
				c.Abort()
			}
		}()
		c.Next()
	}
}

// maxRequestIDLength is the longest client-supplied X-Request-ID that is
// reused.
const maxRequestIDLength = 64

// RequestIDKey is the gin context key under which RequestID stores the request
// ID. The audit middleware and the handlers read it.
const RequestIDKey = "request_id"

// RequestID adds a unique request ID to each request. A client-supplied
// X-Request-ID is reused when it is a valid ID (see validRequestID);
// otherwise, and when there is none, a new one is generated. The ID goes into
// the logs and the response, so a client value must not carry anything else.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader("X-Request-ID")
		if !validRequestID(requestID) {
			requestID = generateRequestID()
		}
		c.Set(RequestIDKey, requestID)
		c.Writer.Header().Set("X-Request-ID", requestID)
		c.Next()
	}
}

// validRequestID reports whether id is 1 to maxRequestIDLength characters of
// [A-Za-z0-9._-].
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		switch b := id[i]; {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '.', b == '_', b == '-':
		default:
			return false
		}
	}
	return true
}

// maxBytesBodyCapture wraps an io.ReadCloser to detect *http.MaxBytesError.
type maxBytesBodyCapture struct {
	rc       io.ReadCloser
	exceeded *bool
}

func (m *maxBytesBodyCapture) Read(p []byte) (int, error) {
	n, err := m.rc.Read(p)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			*m.exceeded = true
		}
	}
	return n, err
}

func (m *maxBytesBodyCapture) Close() error {
	return m.rc.Close()
}

// MaxBodySize limits the size of the request body to prevent memory exhaustion.
// Oversized payloads are translated to a 413 Request Entity Too Large response.
func MaxBodySize(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		// A body that declares a length over the limit is refused before the
		// handler runs. For a body of unknown length (chunked), the handler
		// sees a read error; see handlers.respondBindError.
		if c.Request.ContentLength > maxBytes {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge,
				gin.H{"error": "request body too large"})
			return
		}

		var exceeded bool
		limitedReader := http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Request.Body = &maxBytesBodyCapture{rc: limitedReader, exceeded: &exceeded}
		c.Next()

		// If the body size was exceeded and the handler has not yet written
		// a response, return a 413 so clients get a clear signal.
		if exceeded && !c.Writer.Written() {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge,
				gin.H{"error": "request body too large"})
			return
		}
	}
}

// SecurityHeaders adds standard security headers to all responses.
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")

		// Add HSTS when behind TLS (direct or via reverse proxy).
		isTLS := c.Request.TLS != nil
		if forwardedProto := c.GetHeader("X-Forwarded-Proto"); !isTLS && forwardedProto != "" {
			// X-Forwarded-Proto can be comma-separated (multiple proxies);
			// check the first value. Scheme values are case-insensitive
			// per RFC 3986.
			first := strings.TrimSpace(strings.SplitN(forwardedProto, ",", 2)[0])
			isTLS = strings.EqualFold(first, "https")
		}
		if isTLS {
			c.Header("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}

		c.Next()
	}
}

func generateRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: use timestamp-based ID if crypto/rand fails
		slog.Warn("failed to generate random request ID, using fallback", "error", err)
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
