package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gorilla "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A server-level SERVER_WRITE_TIMEOUT does not end a WebSocket: net/http
// clears the connection deadlines when the handler hijacks it, so the
// deadline that the server set for the upgrade request is gone. This is why
// Compose can pass SERVER_WRITE_TIMEOUT=15s (slow-client protection for REST)
// while the default stays 0.
func TestServerWriteTimeoutDoesNotEndAWebSocket(t *testing.T) {
	t.Parallel()
	const writeTimeout = 100 * time.Millisecond

	upgrader := gorilla.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Write after the write timeout has long passed.
		time.Sleep(3 * writeTimeout)
		_ = conn.WriteMessage(gorilla.TextMessage, []byte("late"))
	}))
	srv.Config.WriteTimeout = writeTimeout
	srv.Start()
	t.Cleanup(srv.Close)

	client, _, err := gorilla.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))

	_, msg, err := client.ReadMessage()
	require.NoError(t, err, "the socket outlived the server write timeout")
	assert.Equal(t, "late", string(msg))
}
