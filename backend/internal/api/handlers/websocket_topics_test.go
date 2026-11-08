package handlers

import (
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

// newWSTestServer serves the handler on /ws and returns the ws:// URL.
func newWSTestServer(t *testing.T, handler *WebSocketHandler) string {
	t.Helper()
	router := gin.New()
	router.GET("/ws", handler.HandleWebSocket)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
}

func TestHandleWebSocket_IdentityUserSendAndDisconnect(t *testing.T) {
	t.Parallel()

	hub := websocket.NewHub()
	go hub.Run()
	defer hub.Shutdown()

	handler := NewWebSocketHandler(hub, "*")
	handler.SetIdentityResolver(func(c *gin.Context) websocket.ClientIdentity {
		return websocket.ClientIdentity{UserID: c.Query("user")}
	})
	wsURL := newWSTestServer(t, handler)

	dial := func(user string) *gorilla.Conn {
		conn, _, err := gorilla.DefaultDialer.Dial(wsURL+"?user="+user, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	u1 := dial("u1")
	u2 := dial("u2")
	waitForHubClients(t, hub, 2)

	hub.BroadcastToUser("u1", []byte("only-u1"))
	require.NoError(t, u1.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, got, err := u1.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, "only-u1", string(got))

	// u2 gets nothing from the u1 message: its first frame is the close frame
	// that DisconnectUser("u2") triggers.
	assert.Equal(t, 1, hub.DisconnectUser("u2"))
	require.NoError(t, u2.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, _, err = u2.ReadMessage()
	var closeErr *gorilla.CloseError
	require.ErrorAs(t, err, &closeErr)
	assert.Equal(t, gorilla.ClosePolicyViolation, closeErr.Code)
	waitForHubClients(t, hub, 1)
}

func TestHandleWebSocket_NilResolverMeansEmptyIdentity(t *testing.T) {
	t.Parallel()

	hub := websocket.NewHub()
	go hub.Run()
	defer hub.Shutdown()

	handler := NewWebSocketHandler(hub, "*")
	handler.SetIdentityResolver(nil)
	wsURL := newWSTestServer(t, handler)

	conn, _, err := gorilla.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()
	waitForHubClients(t, hub, 1)

	// An anonymous socket matches no user, token or key.
	assert.Equal(t, 0, hub.DisconnectUser(""))
	hub.BroadcastToUser("", []byte("x"))
	assert.Equal(t, 1, hub.ClientCount())
}

func TestHandleWebSocket_SubscribeTopic(t *testing.T) {
	t.Parallel()

	hub := websocket.NewHub()
	go hub.Run()
	defer hub.Shutdown()

	wsURL := newWSTestServer(t, NewWebSocketHandler(hub, "*"))
	conn, _, err := gorilla.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()
	waitForHubClients(t, hub, 1)

	require.NoError(t, conn.WriteMessage(gorilla.TextMessage, []byte(`{"type":"subscribe","payload":{"topic":"items"}}`)))

	// Publish until the subscription is in place; the other topic must never
	// arrive.
	var received []string
	require.Eventually(t, func() bool {
		hub.BroadcastToTopic("other", []byte("wrong"))
		hub.BroadcastToTopic("items", []byte("right"))
		if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
			return false
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return false
		}
		received = append(received, string(msg))
		return true
	}, 3*time.Second, 10*time.Millisecond)
	assert.Equal(t, "right", received[0])
}

func TestMockBroadcastSender_ImplementsSenders(t *testing.T) {
	t.Parallel()

	var (
		_ websocket.BroadcastSender = (*MockBroadcastSender)(nil)
		_ websocket.TopicSender     = (*MockBroadcastSender)(nil)
		_ websocket.UserSender      = (*MockBroadcastSender)(nil)
	)
}

func TestMockBroadcastSender_RecordsTargetedMessages(t *testing.T) {
	t.Parallel()

	m := &MockBroadcastSender{}
	m.Broadcast([]byte("all"))
	m.BroadcastToTopic("t1", []byte("topic"))
	m.BroadcastToUser("u1", []byte("user"))

	assert.Equal(t, [][]byte{[]byte("all")}, m.Messages(), "targeted sends are not broadcasts")
	assert.Equal(t, []TargetedMessage{{Target: "t1", Message: []byte("topic")}}, m.TopicMessages())
	assert.Equal(t, []TargetedMessage{{Target: "u1", Message: []byte("user")}}, m.UserMessages())

	m.Reset()
	assert.Empty(t, m.Messages())
	assert.Empty(t, m.TopicMessages())
	assert.Empty(t, m.UserMessages())
}
