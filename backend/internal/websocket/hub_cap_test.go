package websocket

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gorilla "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCappedHub(t *testing.T, limit int) *Hub {
	t.Helper()
	hub := NewHub()
	hub.SetMaxConnectionsPerIdentity(limit)
	go hub.Run()
	t.Cleanup(hub.Shutdown)
	return hub
}

func tryRegister(hub *Hub, id ClientIdentity) (*Client, error) {
	c := &Client{hub: hub, identity: id, send: make(chan []byte, 1)}
	return c, hub.Register(c)
}

func identityConnCount(h *Hub) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.identityConns)
}

func TestHub_DefaultCapPerIdentity(t *testing.T) {
	t.Parallel()
	hub := NewHub()
	assert.EqualValues(t, DefaultMaxConnectionsPerIdentity, hub.maxPerIdentity.Load())
	assert.Equal(t, 16, DefaultMaxConnectionsPerIdentity)
}

func TestHub_RegisterRefusesTheSocketOverTheCap(t *testing.T) {
	t.Parallel()
	hub := newCappedHub(t, 3)

	for i := 0; i < 3; i++ {
		_, err := tryRegister(hub, ClientIdentity{UserID: "u1", TokenID: "jti"})
		require.NoError(t, err)
	}
	refused, err := tryRegister(hub, ClientIdentity{UserID: "u1", TokenID: "jti-other"})
	require.ErrorIs(t, err, ErrTooManyConnections)
	assert.Equal(t, 3, hub.ClientCount(), "the refused client is not registered")
	hub.Subscribe(refused, "items")
	assert.Zero(t, hub.TopicSubscriberCount("items"), "and cannot subscribe")

	// Another user is not affected.
	for i := 0; i < 3; i++ {
		_, err := tryRegister(hub, ClientIdentity{UserID: "u2"})
		require.NoError(t, err)
	}
	// Neither is a socket without identity (authentication off): the base behaviour.
	for i := 0; i < 20; i++ {
		_, err := tryRegister(hub, ClientIdentity{})
		require.NoError(t, err)
	}
	assert.Equal(t, 26, hub.ClientCount())
}

// TestHub_CapAppliesPerAPIKey: the key counts on its own, and the sockets that
// a key opens count for its user too.
func TestHub_CapAppliesPerAPIKey(t *testing.T) {
	t.Parallel()
	hub := newCappedHub(t, 2)

	for i := 0; i < 2; i++ {
		_, err := tryRegister(hub, ClientIdentity{APIKeyID: "k1"})
		require.NoError(t, err)
	}
	_, err := tryRegister(hub, ClientIdentity{APIKeyID: "k1"})
	require.ErrorIs(t, err, ErrTooManyConnections)
	_, err = tryRegister(hub, ClientIdentity{APIKeyID: "k2"})
	require.NoError(t, err, "another key is not affected")

	// The user of two keys cannot get around the user cap with more keys.
	for _, key := range []string{"ka", "kb"} {
		_, err := tryRegister(hub, ClientIdentity{UserID: "u1", APIKeyID: key})
		require.NoError(t, err)
	}
	_, err = tryRegister(hub, ClientIdentity{UserID: "u1", APIKeyID: "kc"})
	require.ErrorIs(t, err, ErrTooManyConnections)
	_, err = tryRegister(hub, ClientIdentity{UserID: "u1", TokenID: "jti"})
	require.ErrorIs(t, err, ErrTooManyConnections)
}

func TestHub_ClosingASocketFreesTheSlot(t *testing.T) {
	t.Parallel()
	hub := newCappedHub(t, 2)
	id := ClientIdentity{UserID: "u1", TokenID: "jti-1", APIKeyID: ""}

	a, _ := tryRegister(hub, id)
	b, _ := tryRegister(hub, id)
	_, err := tryRegister(hub, id)
	require.ErrorIs(t, err, ErrTooManyConnections)

	// Unregister (the read pump of a closed socket).
	hub.Unregister(a)
	waitForClientCount(t, hub, 1)
	c, err := tryRegister(hub, id)
	require.NoError(t, err, "the unregistered socket freed its slot")
	_, err = tryRegister(hub, id)
	require.ErrorIs(t, err, ErrTooManyConnections)

	// A revocation frees the slots too.
	assert.Equal(t, 2, hub.DisconnectToken("jti-1"), "both sockets of the token closed")
	assert.Zero(t, hub.ClientCount())
	_, err = tryRegister(hub, id)
	require.NoError(t, err)

	// An unregister of a client that a revocation already removed changes
	// nothing (the hub loop handles the calls in order, so the last call
	// returns after the earlier ones were processed).
	hub.Unregister(b)
	hub.Unregister(c)
	hub.Unregister(c)
	hub.Unregister(c)
	assert.Equal(t, 1, hub.ClientCount())
	_, err = tryRegister(hub, id)
	require.NoError(t, err)
	_, err = tryRegister(hub, id)
	require.ErrorIs(t, err, ErrTooManyConnections, "the removed clients were not counted off twice")
}

// TestHub_NoCounterLeftBehind: the per-identity counters do not grow with the
// number of identities that came and went.
func TestHub_NoCounterLeftBehind(t *testing.T) {
	t.Parallel()
	hub := newCappedHub(t, 2)
	var clients []*Client
	for i := 0; i < 50; i++ {
		id := ClientIdentity{UserID: fmt.Sprintf("user-%d", i), APIKeyID: fmt.Sprintf("key-%d", i)}
		c, err := tryRegister(hub, id)
		require.NoError(t, err)
		clients = append(clients, c)
		_, err = tryRegister(hub, id) // a second socket for the same identity
		require.NoError(t, err)
	}
	assert.Equal(t, 100, identityConnCount(hub), "a user and a key counter per identity")
	for i, c := range clients {
		hub.DisconnectClient(c)
		if i%2 == 0 {
			continue
		}
		hub.DisconnectUser(fmt.Sprintf("user-%d", i)) // the second socket
	}
	for i := 0; i < 50; i += 2 {
		hub.DisconnectAPIKey(fmt.Sprintf("key-%d", i))
	}
	assert.Zero(t, hub.ClientCount())
	assert.Zero(t, identityConnCount(hub), "every counter is gone when its identity has no socket")
}

func TestHub_ShutdownClearsTheCounters(t *testing.T) {
	t.Parallel()
	hub := NewHub()
	done := make(chan struct{})
	go func() { hub.Run(); close(done) }()
	_, err := tryRegister(hub, ClientIdentity{UserID: "u1"})
	require.NoError(t, err)
	hub.Shutdown()
	<-done
	assert.Zero(t, identityConnCount(hub))
	_, err = tryRegister(hub, ClientIdentity{UserID: "u1"})
	assert.ErrorIs(t, err, ErrHubClosed, "the shutdown check still comes first")
}

// TestHub_CapHoldsUnderConcurrentRegistrations: the check and the registration
// are one step, so concurrent upgrades of one identity never exceed the cap.
func TestHub_CapHoldsUnderConcurrentRegistrations(t *testing.T) {
	t.Parallel()
	const limit, attempts = 5, 100
	hub := newCappedHub(t, limit)

	var ok, refused atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := tryRegister(hub, ClientIdentity{UserID: "u1"})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrTooManyConnections):
				refused.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.EqualValues(t, limit, ok.Load())
	assert.EqualValues(t, attempts-limit, refused.Load())
	assert.Equal(t, limit, hub.ClientCount())
}

func TestHub_SetMaxConnectionsPerIdentityZeroDisablesTheCap(t *testing.T) {
	t.Parallel()
	hub := newCappedHub(t, 0)
	for i := 0; i < 40; i++ {
		_, err := tryRegister(hub, ClientIdentity{UserID: "u1"})
		require.NoError(t, err)
	}
}

// capServer upgrades every request and hands the socket to NewClientWithIdentity
// with the identity of the X-Test-User header.
func capServer(t *testing.T, hub *Hub) string {
	t.Helper()
	up := gorilla.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_, _ = NewClientWithIdentity(hub, conn, ClientIdentity{UserID: r.Header.Get("X-Test-User")})
	}))
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http")
}

func dialAs(t *testing.T, url, user string) *gorilla.Conn {
	t.Helper()
	h := http.Header{}
	h.Set("X-Test-User", user)
	conn, _, err := gorilla.DefaultDialer.Dial(url, h)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestClient_RefusedSocketGetsClose1008: the socket over the cap is told why
// (policy violation) and closed; it starts no pump.
func TestClient_RefusedSocketGetsClose1008(t *testing.T) {
	t.Parallel()
	hub := newCappedHub(t, 2)
	url := capServer(t, hub)

	var open []*gorilla.Conn
	for i := 0; i < 2; i++ {
		open = append(open, dialAs(t, url, "u1"))
	}
	waitForClientCount(t, hub, 2)

	refused := dialAs(t, url, "u1")
	closeErr := readCloseError(t, refused)
	assert.Equal(t, gorilla.ClosePolicyViolation, closeErr.Code)
	assert.Equal(t, "too many connections", closeErr.Text)
	assert.Equal(t, 2, hub.ClientCount())

	other := dialAs(t, url, "u2")
	waitForClientCount(t, hub, 3)
	require.NoError(t, other.WriteMessage(gorilla.TextMessage, []byte(`{}`)))

	// Closing one of the open sockets frees the slot.
	require.NoError(t, open[0].Close())
	waitForClientCount(t, hub, 2)
	again := dialAs(t, url, "u1")
	waitForClientCount(t, hub, 3)
	require.NoError(t, again.WriteMessage(gorilla.TextMessage, []byte(`{}`)))
}

// TestClient_RefusalPathLeaksNoGoroutine: a flood of refused upgrades leaves
// no goroutine and no registration behind.
func TestClient_RefusalPathLeaksNoGoroutine(t *testing.T) {
	// Not parallel: it compares the goroutine count of the process.
	hub := newCappedHub(t, 1)
	url := capServer(t, hub)
	first := dialAs(t, url, "u1")
	waitForClientCount(t, hub, 1)
	_ = first

	// Warm up the HTTP machinery, then measure.
	for i := 0; i < 3; i++ {
		c := dialAs(t, url, "u1")
		readCloseError(t, c)
		_ = c.Close()
	}
	before := runtime.NumGoroutine()
	for i := 0; i < 40; i++ {
		c := dialAs(t, url, "u1")
		readCloseError(t, c)
		_ = c.Close()
	}
	assert.Eventually(t, func() bool { return runtime.NumGoroutine() <= before+3 }, 3*time.Second, 20*time.Millisecond,
		"goroutines before %d, after %d", before, runtime.NumGoroutine())
	assert.Equal(t, 1, hub.ClientCount())
	assert.Equal(t, 1, identityConnCount(hub), "the counter of the one open socket only")
}

// TestHub_RefusalIsLoggedOnceAndBounded: one Warn per interval with no more
// than the identifiers, the others at Debug.
func TestHub_RefusalIsLoggedOnceAndBounded(t *testing.T) {
	// Not parallel: it replaces the default slog logger.
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	hub := newCappedHub(t, 1)
	_, err := tryRegister(hub, ClientIdentity{UserID: "u1", TokenID: "secret-jti"})
	require.NoError(t, err)
	for i := 0; i < 30; i++ {
		_, err := tryRegister(hub, ClientIdentity{UserID: "u1", TokenID: "secret-jti"})
		require.ErrorIs(t, err, ErrTooManyConnections)
	}

	out := buf.String()
	warn, debug := 0, 0
	for _, l := range strings.Split(out, "\n") {
		if !strings.Contains(l, "WebSocket connection limit reached") {
			continue
		}
		switch {
		case strings.Contains(l, "level=WARN"):
			warn++
		case strings.Contains(l, "level=DEBUG"):
			debug++
		}
	}
	assert.Equal(t, 1, warn)
	assert.Equal(t, 29, debug)
	assert.Contains(t, out, "user_id=u1")
	assert.NotContains(t, out, "secret-jti", "no token identifier in the log")
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
