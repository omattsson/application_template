package websocket

import (
	"errors"
	"testing"
	"time"

	gorilla "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isClosed reports whether ch is closed. It does not block. A test that calls
// it must not send on ch.
func isClosed(ch chan []byte) bool {
	select {
	case _, ok := <-ch:
		return !ok
	default:
		return false
	}
}

func TestHub_Disconnect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		disconnect func(h *Hub) int
		wantClosed []int // indexes into the clients below
	}{
		{name: "user", disconnect: func(h *Hub) int { return h.DisconnectUser("u1") }, wantClosed: []int{0, 1, 4}},
		{name: "token", disconnect: func(h *Hub) int { return h.DisconnectToken("jti-b") }, wantClosed: []int{1}},
		{name: "api key", disconnect: func(h *Hub) int { return h.DisconnectAPIKey("key-1") }, wantClosed: []int{4}},
		{name: "unknown api key", disconnect: func(h *Hub) int { return h.DisconnectAPIKey("key-x") }},
		{name: "empty api key ID", disconnect: func(h *Hub) int { return h.DisconnectAPIKey("") }},
		{name: "unknown user", disconnect: func(h *Hub) int { return h.DisconnectUser("nobody") }},
		{name: "empty user ID", disconnect: func(h *Hub) int { return h.DisconnectUser("") }},
		{name: "empty token ID", disconnect: func(h *Hub) int { return h.DisconnectToken("") }},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hub := NewHub()
			go hub.Run()
			defer hub.Shutdown()

			identities := []ClientIdentity{
				{UserID: "u1", TokenID: "jti-a"},
				{UserID: "u1", TokenID: "jti-b"},
				{UserID: "u2", TokenID: "jti-c"},
				{}, // socket without identity
				{UserID: "u1", APIKeyID: "key-1"},
			}
			clients := make([]*Client, len(identities))
			for i, id := range identities {
				clients[i] = newRegistered(t, hub, id, 1)
				hub.Subscribe(clients[i], "topic-1")
			}

			closed := tt.disconnect(hub)

			assert.Equal(t, len(tt.wantClosed), closed)
			assert.Equal(t, len(clients)-len(tt.wantClosed), hub.ClientCount())
			want := map[int]bool{}
			for _, i := range tt.wantClosed {
				want[i] = true
			}
			for i, c := range clients {
				assert.Equal(t, want[i], isClosed(c.send), "client %d send closed", i)
				if want[i] {
					assert.Equal(t, closeReasonRevoked, c.closeReason)
				}
			}

			// The revoked clients no longer get topic messages. A send on a
			// closed channel would panic here.
			hub.BroadcastToTopic("topic-1", []byte("msg"))
			assert.Equal(t, len(clients)-len(tt.wantClosed), topicSubscriberCount(hub, "topic-1"))
		})
	}
}

func TestHub_DisconnectNilHub(t *testing.T) {
	t.Parallel()

	var hub *Hub
	var revoker ClientRevoker = hub
	assert.Equal(t, 0, revoker.DisconnectUser("u1"))
	assert.Equal(t, 0, revoker.DisconnectToken("jti"))
	assert.Equal(t, 0, revoker.DisconnectAPIKey("key"))
}

// readCloseError reads from peer until it gets a close frame and returns it.
func readCloseError(t *testing.T, peer *gorilla.Conn) *gorilla.CloseError {
	t.Helper()
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		_, _, err := peer.ReadMessage()
		if err == nil {
			continue
		}
		var closeErr *gorilla.CloseError
		require.True(t, errors.As(err, &closeErr), "want a close frame, got %v", err)
		return closeErr
	}
}

func TestClient_CloseFrames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		identity   func() ClientIdentity
		revoke     func(h *Hub)
		wantReason string
	}{
		{
			name: "token expiry",
			identity: func() ClientIdentity {
				return ClientIdentity{UserID: "u1", TokenID: "jti-1", ExpiresAt: time.Now().Add(150 * time.Millisecond)}
			},
			wantReason: closeReasonExpired,
		},
		{
			name:       "token already expired",
			identity:   func() ClientIdentity { return ClientIdentity{UserID: "u1", ExpiresAt: time.Now().Add(-time.Second)} },
			wantReason: closeReasonExpired,
		},
		{
			name: "user revoked",
			identity: func() ClientIdentity {
				return ClientIdentity{UserID: "u1", TokenID: "jti-1", ExpiresAt: time.Now().Add(time.Hour)}
			},
			revoke:     func(h *Hub) { h.DisconnectUser("u1") },
			wantReason: closeReasonRevoked,
		},
		{
			name:       "token revoked",
			identity:   func() ClientIdentity { return ClientIdentity{UserID: "u1", TokenID: "jti-1"} },
			revoke:     func(h *Hub) { h.DisconnectToken("jti-1") },
			wantReason: closeReasonRevoked,
		},
		{
			name: "api key expires",
			identity: func() ClientIdentity {
				return ClientIdentity{UserID: "u1", APIKeyID: "key-1", ExpiresAt: time.Now().Add(100 * time.Millisecond)}
			},
			wantReason: closeReasonAPIKeyExpired,
		},
		{
			name:       "api key revoked",
			identity:   func() ClientIdentity { return ClientIdentity{UserID: "u1", APIKeyID: "key-1"} },
			revoke:     func(h *Hub) { h.DisconnectAPIKey("key-1") },
			wantReason: closeReasonRevoked,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hub := NewHub()
			go hub.Run()
			defer hub.Shutdown()

			srvConn, peerConn := newTestWSPair(t)
			_, err := NewClientWithIdentity(hub, srvConn, tt.identity())
			require.NoError(t, err)

			if tt.revoke != nil {
				tt.revoke(hub)
			}

			closeErr := readCloseError(t, peerConn)
			assert.Equal(t, gorilla.ClosePolicyViolation, closeErr.Code)
			assert.Equal(t, tt.wantReason, closeErr.Text)
			waitForClientCount(t, hub, 0)
		})
	}
}

func TestClient_NoExpiryWithoutIdentity(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	srvConn, peerConn := newTestWSPair(t)
	_, err := NewClient(hub, srvConn)
	require.NoError(t, err)

	// A broadcast still arrives: a zero expiry does not close the socket.
	hub.Broadcast([]byte(`{"type":"ping"}`))
	require.NoError(t, peerConn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, msg, err := peerConn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, `{"type":"ping"}`, string(msg))
}
