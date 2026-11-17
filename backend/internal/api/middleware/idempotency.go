package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"backend/internal/ctxutil"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
)

// Names and limits of the idempotency module.
const (
	// IdempotencyKeyHeader is the request header that carries the key.
	IdempotencyKeyHeader = "Idempotency-Key"
	// IdempotencyReplayedHeader is set to "true" on a replayed response.
	IdempotencyReplayedHeader = "Idempotency-Replayed"
	// MaxIdempotencyKeyLen is the longest key in bytes.
	MaxIdempotencyKeyLen = 255
	// MaxStoredResponseBytes is the largest response body that is stored
	// (256 KiB). A larger response is sent but not stored, and the key is
	// released.
	MaxStoredResponseBytes = 256 << 10
)

// Messages of the idempotency answers.
const (
	msgIdempotencyKeyInvalid = "Idempotency-Key must be 1 to 255 printable ASCII characters"
	msgIdempotencyMismatch   = "Idempotency-Key was already used with a different request"
	msgIdempotencyInProgress = "A request with this Idempotency-Key is still in progress"
	msgInvalidRequestFormat  = "Invalid request format"
	msgRequestBodyTooLarge   = "request body too large"
)

// IdempotencyOptions are the settings of Idempotency.
type IdempotencyOptions struct {
	// Store keeps the keys and the stored responses (required).
	Store models.IdempotencyRepository
	// SkipPathPrefixes are the paths that are never touched, such as the
	// /auth/ routes: a replayed login would replay tokens.
	SkipPathPrefixes []string
	// TTL is the life of a stored response.
	TTL time.Duration
	// LockTimeout is how long a key in progress blocks a retry.
	LockTimeout time.Duration
	// PerUser scopes a key to the authenticated user (auth on). A request
	// without a user then fails with 500: the middleware must run after the
	// authentication. With false all clients share one scope (auth off).
	PerUser bool
}

// Idempotency makes a POST with an Idempotency-Key header safe to retry. The
// first request with a key claims it in the store (MySQL arbitrates between
// replicas), runs the handler and stores the status, Content-Type and body of
// the response. A repeat with the same user, method, path, query and body
// replays that response with Idempotency-Replayed: true and does not run the
// handler (it writes no audit entry either: SkipAudit). The same key with
// another request answers 422; the same key while the first request still
// runs, and its lock has not timed out, answers 409 with Retry-After.
//
// Only POST is touched. A request without the header, and a path in
// SkipPathPrefixes, pass through unchanged. A key that is empty, longer than
// 255 bytes or not printable ASCII answers 400.
//
// A 5xx or 499 response, a response over MaxStoredResponseBytes, a flushed
// response and a handler panic release the key, so the client can retry; 2xx,
// 3xx and 4xx responses are stored. The response of a request that claimed a
// key is held back until it is stored (or the key released), so a client that
// retries after it saw the response always gets a replay. The store and the
// release run on a detached context with a deadline (ctxutil.Detach): a client
// that leaves cannot skip them. A store of the response that fails is logged
// and the response is sent anyway: the key then times out like a crashed one.
// A failing claim answers 500 and never runs the handler unprotected.
//
// Put it after the authentication (the user is the scope) and the audit
// recorder (it reads the status after the handler).
func Idempotency(opts IdempotencyOptions) gin.HandlerFunc {
	errLog := newStoreErrorLog()

	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost || skipsIdempotency(c.Request.URL.Path, opts.SkipPathPrefixes) {
			c.Next()
			return
		}
		values, present := c.Request.Header[IdempotencyKeyHeader]
		if !present {
			c.Next()
			return
		}
		key, ok := idempotencyKey(values)
		if !ok {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": msgIdempotencyKeyInvalid})
			return
		}

		scope := models.IdempotencyScopeShared
		if opts.PerUser {
			if scope = GetUserIDFromContext(c); scope == "" {
				// Auth is on and no user is known: the middleware is misplaced.
				// Fail closed instead of sharing a scope between users.
				errLog.log(c.Request.Context(), "idempotency: no authenticated user on a request with a key")
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
				return
			}
		}

		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": msgRequestBodyTooLarge})
				return
			}
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": msgInvalidRequestFormat})
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		hash := hashOfRequest(c.Request.Method, c.Request.URL.Path, c.Request.URL.RawQuery, body)

		claim, err := claimKey(c, opts, scope, key, hash)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				c.AbortWithStatusJSON(statusClientClosedRequest, gin.H{"error": msgClientClosed})
				return
			}
			errLog.log(c.Request.Context(), "idempotency: claim failed", "error", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
			return
		}

		switch claim.Outcome {
		case models.IdempotencyReplay:
			SkipAudit(c)
			replay(c, claim.Response)
			return
		case models.IdempotencyMismatch:
			SkipAudit(c)
			c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": msgIdempotencyMismatch})
			return
		case models.IdempotencyInProgress:
			SkipAudit(c)
			c.Header("Retry-After", strconv.Itoa(retryAfterSeconds(claim.RetryAfter)))
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": msgIdempotencyInProgress})
			return
		case models.IdempotencyClaimed:
			runClaimed(c, opts, errLog, scope, key, claim.LockID)
		default:
			errLog.log(c.Request.Context(), "idempotency: unknown claim outcome", "outcome", int(claim.Outcome))
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": msgInternalServerError})
		}
	}
}

// skipsIdempotency reports whether path starts with one of prefixes.
func skipsIdempotency(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// idempotencyKey validates the header values. The header must be present once
// with a key of 1 to MaxIdempotencyKeyLen bytes of printable ASCII (space is
// allowed inside) after the surrounding white space is removed.
func idempotencyKey(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	key := strings.TrimSpace(values[0])
	if key == "" || len(key) > MaxIdempotencyKeyLen {
		return "", false
	}
	for i := 0; i < len(key); i++ {
		if b := key[i]; b < 0x20 || b > 0x7e {
			return "", false
		}
	}
	return key, true
}

// hashOfRequest is the fingerprint of a request: the hex SHA-256 of method,
// path, raw query and body, each followed by a newline (none of the first
// three contains one), so a value cannot shift into its neighbour.
func hashOfRequest(method, path, rawQuery string, body []byte) string {
	h := sha256.New()
	for _, s := range []string{method, path, rawQuery} {
		_, _ = io.WriteString(h, s)
		_, _ = h.Write([]byte{'\n'})
	}
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// retryAfterSeconds is d in whole seconds, at least one.
func retryAfterSeconds(d time.Duration) int {
	secs := int((d + time.Second - 1) / time.Second)
	return max(secs, 1)
}

// claimKey claims the key on a detached context: the claim is the first
// half of a unit (claim, run, complete) that must not be cut by a client who
// leaves.
func claimKey(c *gin.Context, opts IdempotencyOptions, scope, key, hash string) (*models.IdempotencyClaim, error) {
	ctx, cancel := ctxutil.Detach(c.Request.Context(), ctxutil.WriteTimeout)
	defer cancel()
	claim, err := opts.Store.Claim(ctx, scope, key, hash, opts.TTL, opts.LockTimeout)
	if err == nil && claim == nil {
		return nil, errors.New("the store returned no claim")
	}
	return claim, err
}

// replay writes a stored response.
func replay(c *gin.Context, r *models.IdempotencyResponse) {
	if r.ContentType != "" {
		c.Header("Content-Type", r.ContentType)
	}
	c.Header(IdempotencyReplayedHeader, "true")
	c.Status(r.StatusCode)
	if len(r.Body) > 0 {
		_, _ = c.Writer.Write(r.Body)
	}
	c.Writer.WriteHeaderNow()
	c.Abort()
}

// runClaimed runs the rest of the chain for the request that owns the key,
// then stores the response or releases the key.
func runClaimed(c *gin.Context, opts IdempotencyOptions, errLog *storeErrorLog, scope, key, lockID string) {
	orig := c.Writer
	sw := &idemWriter{ResponseWriter: orig, status: http.StatusOK}
	c.Writer = sw

	finished := false
	defer func() {
		if finished {
			return
		}
		// A panic in the chain: Recovery (outside) answers it. Free the key and
		// put the real writer back so its 500 is written to the client.
		c.Writer = orig
		releaseKey(c, opts, errLog, scope, key, lockID)
	}()

	c.Next()
	finished = true
	c.Writer = orig

	status := sw.Status()
	if sw.direct || status >= http.StatusInternalServerError || status == statusClientClosedRequest {
		// Not storable: a failure, or a response that was streamed or is too
		// large. The client retries and the handler runs again.
		releaseKey(c, opts, errLog, scope, key, lockID)
	} else {
		storeResponse(c, opts, errLog, scope, key, lockID, models.IdempotencyResponse{
			StatusCode:  status,
			ContentType: orig.Header().Get("Content-Type"),
			Body:        sw.buf.Bytes(),
		})
	}

	sw.flushTo(orig)
}

// storeResponse stores resp on a detached context.
func storeResponse(c *gin.Context, opts IdempotencyOptions, errLog *storeErrorLog, scope, key, lockID string, resp models.IdempotencyResponse) {
	ctx, cancel := ctxutil.Detach(c.Request.Context(), ctxutil.WriteTimeout)
	defer cancel()
	stored, err := opts.Store.Complete(ctx, scope, key, lockID, resp, opts.TTL)
	switch {
	case err != nil:
		errLog.log(ctx, "idempotency: storing the response failed; the key stays locked until the lock timeout", "error", err)
	case !stored:
		// Another request took the key over while this one ran (it ran longer
		// than IDEMPOTENCY_LOCK_TIMEOUT). Its work is not touched.
		slog.WarnContext(ctx, "idempotency: the key was taken over before the response was stored; raise IDEMPOTENCY_LOCK_TIMEOUT above the longest POST")
	}
}

// releaseKey frees the key on a detached context.
func releaseKey(c *gin.Context, opts IdempotencyOptions, errLog *storeErrorLog, scope, key, lockID string) {
	ctx, cancel := ctxutil.Detach(c.Request.Context(), ctxutil.WriteTimeout)
	defer cancel()
	if err := opts.Store.Release(ctx, scope, key, lockID); err != nil {
		errLog.log(ctx, "idempotency: releasing the key failed; it stays locked until the lock timeout", "error", err)
	}
}

// idemWriter holds the response of the request that owns a key back until it
// is stored. It buffers up to MaxStoredResponseBytes; past that, or on a
// Flush, it writes everything through and the response is not stored (direct).
// It keeps the semantics of gin.ResponseWriter that the chain outside relies
// on: Status, Size and Written describe what the handler did.
type idemWriter struct {
	gin.ResponseWriter
	buf    bytes.Buffer
	status int
	// wrote is true once the status or a byte was written (buffer mode).
	wrote bool
	// direct is true once the response is written through to the client.
	direct bool
}

// goDirect writes the held back status and body to the client and passes
// every later write through.
func (w *idemWriter) goDirect() {
	if w.direct {
		return
	}
	w.direct = true
	w.ResponseWriter.WriteHeader(w.status)
	if w.buf.Len() > 0 {
		_, _ = w.ResponseWriter.Write(w.buf.Bytes())
		w.buf = bytes.Buffer{}
	}
}

// flushTo writes the held back response to the client. It does nothing after
// goDirect, which already did.
func (w *idemWriter) flushTo(dst gin.ResponseWriter) {
	if w.direct {
		return
	}
	dst.WriteHeader(w.status)
	if w.buf.Len() > 0 {
		_, _ = dst.Write(w.buf.Bytes())
	}
}

func (w *idemWriter) WriteHeader(code int) {
	if w.direct {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if code > 0 && !w.wrote {
		w.status = code
	}
}

func (w *idemWriter) WriteHeaderNow() {
	if w.direct {
		w.ResponseWriter.WriteHeaderNow()
		return
	}
	w.wrote = true
}

func (w *idemWriter) Write(b []byte) (int, error) {
	if w.direct {
		return w.ResponseWriter.Write(b)
	}
	w.wrote = true
	if w.buf.Len()+len(b) > MaxStoredResponseBytes {
		w.goDirect()
		return w.ResponseWriter.Write(b)
	}
	return w.buf.Write(b)
}

func (w *idemWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// Flush sends what is held back: a flushed response is a stream, which is
// not stored.
func (w *idemWriter) Flush() {
	w.goDirect()
	w.ResponseWriter.Flush()
}

func (w *idemWriter) Status() int {
	if w.direct {
		return w.ResponseWriter.Status()
	}
	return w.status
}

func (w *idemWriter) Size() int {
	if w.direct {
		return w.ResponseWriter.Size()
	}
	if !w.wrote {
		return -1
	}
	return w.buf.Len()
}

func (w *idemWriter) Written() bool {
	if w.direct {
		return w.ResponseWriter.Written()
	}
	return w.wrote
}
