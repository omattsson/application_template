package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	gorilla "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		allowedOrigins string
		origin         string
		want           bool
	}{
		{
			name:           "wildcard allows any origin",
			allowedOrigins: "*",
			origin:         "http://evil.com",
			want:           true,
		},
		{
			name:           "empty string allows any origin",
			allowedOrigins: "",
			origin:         "http://evil.com",
			want:           true,
		},
		{
			name:           "specific origin allows matching request",
			allowedOrigins: "http://example.com",
			origin:         "http://example.com",
			want:           true,
		},
		{
			name:           "specific origin rejects non-matching request",
			allowedOrigins: "http://example.com",
			origin:         "http://evil.com",
			want:           false,
		},
		{
			name:           "multiple comma-separated origins allow first match",
			allowedOrigins: "http://example.com,http://other.com",
			origin:         "http://example.com",
			want:           true,
		},
		{
			name:           "multiple comma-separated origins allow second match",
			allowedOrigins: "http://example.com,http://other.com",
			origin:         "http://other.com",
			want:           true,
		},
		{
			name:           "multiple origins reject non-matching request",
			allowedOrigins: "http://example.com,http://other.com",
			origin:         "http://evil.com",
			want:           false,
		},
		{
			name:           "comma-separated with spaces trims correctly",
			allowedOrigins: "http://example.com, http://other.com",
			origin:         "http://other.com",
			want:           true,
		},
		{
			name:           "no origin header allows request (same-origin)",
			allowedOrigins: "http://example.com",
			origin:         "",
			want:           true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handler := NewWebSocketHandler(nil, tt.allowedOrigins)
			req, err := http.NewRequest("GET", "/ws", nil)
			require.NoError(t, err)

			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}

			got := handler.checkOrigin(req)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewWebSocketHandler(t *testing.T) {
	t.Parallel()

	hub := websocket.NewHub()
	handler := NewWebSocketHandler(hub, "http://example.com")

	assert.NotNil(t, handler)
	assert.Equal(t, "http://example.com", handler.allowedOrigins)
}

// waitForHubClients polls hub.ClientCount until it equals want or timeout.
func waitForHubClients(t *testing.T, hub *websocket.Hub, want int) {
	t.Helper()
	assert.Eventually(t, func() bool {
		return hub.ClientCount() == want
	}, 2*time.Second, 10*time.Millisecond, "expected %d clients", want)
}

func TestHandleWebSocket_SuccessfulUpgrade(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	hub := websocket.NewHub()
	go hub.Run()
	defer hub.Shutdown()

	handler := NewWebSocketHandler(hub, "*")

	router := gin.New()
	router.GET("/ws", handler.HandleWebSocket)

	server := httptest.NewServer(router)
	defer server.Close()

	// Convert http:// to ws://
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	conn, resp, err := gorilla.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	waitForHubClients(t, hub, 1)
}

func TestHandleWebSocket_BroadcastReceived(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	hub := websocket.NewHub()
	go hub.Run()
	defer hub.Shutdown()

	handler := NewWebSocketHandler(hub, "*")

	router := gin.New()
	router.GET("/ws", handler.HandleWebSocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	conn, _, err := gorilla.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	waitForHubClients(t, hub, 1)

	// Broadcast a message through the hub
	msg := []byte(`{"type":"test","payload":"hello"}`)
	hub.Broadcast(msg)

	// Read the message from the WebSocket connection
	err = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	require.NoError(t, err)

	_, received, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, msg, received)
}

func TestHandleWebSocket_MultipleClients(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	hub := websocket.NewHub()
	go hub.Run()
	defer hub.Shutdown()

	handler := NewWebSocketHandler(hub, "*")

	router := gin.New()
	router.GET("/ws", handler.HandleWebSocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	conn1, _, err := gorilla.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn1.Close()

	conn2, _, err := gorilla.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn2.Close()

	waitForHubClients(t, hub, 2)

	msg := []byte(`{"type":"broadcast","payload":"all"}`)
	hub.Broadcast(msg)

	for i, conn := range []*gorilla.Conn{conn1, conn2} {
		err = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		require.NoError(t, err, "client %d set deadline", i+1)

		_, received, err := conn.ReadMessage()
		require.NoError(t, err, "client %d read", i+1)
		assert.Equal(t, msg, received, "client %d message", i+1)
	}
}

func TestHandleWebSocket_HubShutdown(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	hub := websocket.NewHub()
	go hub.Run()

	handler := NewWebSocketHandler(hub, "*")

	router := gin.New()
	router.GET("/ws", handler.HandleWebSocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	// Connect a client first, then shut down the hub
	conn, _, err := gorilla.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	waitForHubClients(t, hub, 1)

	hub.Shutdown()
	waitForHubClients(t, hub, 0)

	// The connected client should receive a close frame or read error
	err = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	require.NoError(t, err)

	_, _, err = conn.ReadMessage()
	assert.Error(t, err, "read after hub shutdown should fail")
}

func TestHandleWebSocket_HubClosedBeforeUpgrade(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	hub := websocket.NewHub()
	go hub.Run()
	hub.Shutdown()

	handler := NewWebSocketHandler(hub, "*")

	router := gin.New()
	router.GET("/ws", handler.HandleWebSocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	// The upgrade itself succeeds (HTTP → WS), but NewClient fails
	// because the hub is closed. The server closes the connection.
	conn, _, err := gorilla.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		// Connection refused or failed — acceptable when hub is closed
		return
	}
	defer conn.Close()

	// If dial succeeded, the connection should be immediately closed by server
	err = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	require.NoError(t, err)

	_, _, err = conn.ReadMessage()
	assert.Error(t, err, "connection should be closed when hub is shut down")
}

func TestHandleWebSocket_OriginRejected(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	hub := websocket.NewHub()
	go hub.Run()
	defer hub.Shutdown()

	handler := NewWebSocketHandler(hub, "http://allowed.com")

	router := gin.New()
	router.GET("/ws", handler.HandleWebSocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	// Dial with a disallowed origin
	header := http.Header{}
	header.Set("Origin", "http://evil.com")

	_, resp, err := gorilla.DefaultDialer.Dial(wsURL, header)
	assert.Error(t, err, "dial with rejected origin should fail")
	if resp != nil {
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	}
}

func TestHandleWebSocket_OriginAllowed(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	hub := websocket.NewHub()
	go hub.Run()
	defer hub.Shutdown()

	handler := NewWebSocketHandler(hub, "http://allowed.com")

	router := gin.New()
	router.GET("/ws", handler.HandleWebSocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	header := http.Header{}
	header.Set("Origin", "http://allowed.com")

	conn, resp, err := gorilla.DefaultDialer.Dial(wsURL, header)
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	waitForHubClients(t, hub, 1)
}

func TestHandleWebSocket_ClientDisconnect(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	hub := websocket.NewHub()
	go hub.Run()
	defer hub.Shutdown()

	handler := NewWebSocketHandler(hub, "*")

	router := gin.New()
	router.GET("/ws", handler.HandleWebSocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	conn, _, err := gorilla.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)

	waitForHubClients(t, hub, 1)

	// Client closes the connection
	conn.Close()

	// Hub should eventually unregister the client
	waitForHubClients(t, hub, 0)
}
