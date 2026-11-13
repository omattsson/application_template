package middleware

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureLogs makes a debug-level text logger the default one and returns its
// output. The default logger is process-wide: not for parallel tests.
func captureLogs(t *testing.T) *syncBuf {
	t.Helper()
	var buf syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// lines returns the log lines of out that contain every one of parts.
func lines(out string, parts ...string) []string {
	var found []string
	for _, l := range strings.Split(out, "\n") {
		ok := l != ""
		for _, p := range parts {
			ok = ok && strings.Contains(l, p)
		}
		if ok {
			found = append(found, l)
		}
	}
	return found
}

// TestJWTStoreErrorDoesNotFloodTheLog: with the session store down every
// request fails the same lookup. One Error line per interval is logged (with
// the count of the others); the rest go to Debug, and the answers do not
// change.
func TestJWTStoreErrorDoesNotFloodTheLog(t *testing.T) {
	buf := captureLogs(t)
	memory := sessionstore.NewMemory()
	t.Cleanup(memory.Stop)
	store := failingStore{memory, errors.New("connection refused to db.internal:3306")}

	for _, strict := range []bool{false, true} {
		r := jwtTestRouter(JWTAuthOptions{JWTSecret: testSecret, SessionStore: store, FailClosedOnStoreError: strict})
		wantCode := http.StatusOK
		if strict {
			wantCode = http.StatusInternalServerError
		}
		for i := 0; i < 50; i++ {
			w := doGet(r, "/protected", bearer(t, validClaims()))
			require.Equal(t, wantCode, w.Code)
		}
	}

	out := buf.String()
	errLines := lines(out, "level=ERROR", "Failed to check token")
	assert.Len(t, errLines, 2, "one Error line per middleware (fail open and fail closed), not one per request")
	assert.Len(t, lines(out, "level=DEBUG", "Failed to check token"), 98, "the other requests are logged at Debug, so nothing is lost at LOG_LEVEL=debug")
}

func TestStoreErrorLogReportsHowManyWereSuppressed(t *testing.T) {
	buf := captureLogs(t)

	now := time.Unix(1000, 0)
	l := newStoreErrorLogWithClock(func() time.Time { return now })
	for i := 0; i < 4; i++ {
		l.log(context.Background(), "msg", "k", "v")
	}
	now = now.Add(storeErrorLogInterval) // the interval has passed
	l.log(context.Background(), "msg", "k", "v")

	errLines := lines(buf.String(), "level=ERROR", "msg=msg")
	require.Len(t, errLines, 2)
	assert.NotContains(t, errLines[0], "suppressed")
	assert.Contains(t, errLines[1], "suppressed=3")
	assert.Len(t, lines(buf.String(), "level=DEBUG", "msg=msg"), 3)
}

// TestAPIKeyStoreErrorDoesNotFloodTheLog: the same for the API key lookups.
func TestAPIKeyStoreErrorDoesNotFloodTheLog(t *testing.T) {
	buf := captureLogs(t)
	client, rec := issuedKey(t, "k1", "u1", inHours(24))
	users := newUserRepoFake(&models.User{ID: "u1", Username: "alice", Role: models.RoleUser})
	keys := newKeyRepoFake(rec)
	keys.prefixErr = dberrors.NewDatabaseError("find", errors.New("dial tcp 10.0.0.5:3306: refused"))
	r := keyRouter(keyDeps(keys, users))

	for i := 0; i < 50; i++ {
		w := doGet(r, "/protected", map[string]string{"X-API-Key": client})
		require.Equal(t, http.StatusUnauthorized, w.Code, "the answer is the same as before")
	}

	out := buf.String()
	assert.Len(t, lines(out, "level=ERROR", "API key lookup failed"), 1)
	assert.Len(t, lines(out, "level=DEBUG", "API key lookup failed"), 49)
	assert.NotContains(t, out, client, "the key is never logged")
}

func TestLimiterIsPerStoreErrorLogInstance(t *testing.T) {
	a, b := newStoreErrorLog(), newStoreErrorLog()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a.log(context.Background(), "from a")
	b.log(context.Background(), "from b")
	a.log(context.Background(), "from a again")

	assert.Contains(t, buf.String(), "from a")
	assert.Contains(t, buf.String(), "from b", "another middleware has its own budget")
	assert.NotContains(t, buf.String(), "from a again")
}
