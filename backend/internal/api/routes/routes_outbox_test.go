package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"backend/internal/api/handlers"
	"backend/internal/health"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveItems builds the router with the outbox switch set and returns the
// URL of a server for it and its hub.
func serveItems(t *testing.T, outboxOn bool) (base string, wsTarget string) {
	t.Helper()
	cfg := testConfig(1000, 10, nil)
	cfg.Outbox.Enabled = outboxOn

	router := gin.New()
	hc := health.New()
	hc.SetReady(true)
	hub := websocket.NewHub()
	go hub.Run()
	t.Cleanup(hub.Shutdown)

	res, err := Setup(router, Deps{Config: cfg, Repository: handlers.NewMockRepository(), HealthChecker: hc, Hub: hub})
	require.NoError(t, err)
	t.Cleanup(res.RateLimiters.Stop)

	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv.URL, "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
}

func createItem(t *testing.T, base string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/items", strings.NewReader(`{"name":"pen","price":1.5}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}

// With the outbox off the handler broadcasts directly, as before.
func TestItemsBroadcastDirectlyWithTheOutboxOff(t *testing.T) {
	t.Parallel()
	base, target := serveItems(t, false)
	conn, _ := dialWS(t, target, nil)
	// The hub registers the client asynchronously.
	time.Sleep(100 * time.Millisecond)

	createItem(t, base)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(msg), `"type":"item.created"`)
}

// With the outbox on the handler never broadcasts: the poller delivers the
// event from the table, so a direct send would give each socket two copies.
func TestItemsDoNotBroadcastDirectlyWithTheOutboxOn(t *testing.T) {
	t.Parallel()
	base, target := serveItems(t, true)
	conn, _ := dialWS(t, target, nil)
	time.Sleep(100 * time.Millisecond)

	createItem(t, base)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
	_, _, err := conn.ReadMessage()
	require.Error(t, err, "no message without the poller")
	var ne interface{ Timeout() bool }
	require.ErrorAs(t, err, &ne)
	assert.True(t, ne.Timeout())
}
