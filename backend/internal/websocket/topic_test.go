package websocket

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recv returns the next message on ch or fails the test after a short wait.
func recv(t *testing.T, ch chan []byte) []byte {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(time.Second):
		t.Fatal("no message received in time")
		return nil
	}
}

// noMessage fails when ch has a queued message. The hub delivers topic and
// user messages synchronously, so an immediate check is enough.
func noMessage(t *testing.T, ch chan []byte, msg string) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("%s: unexpected message %q", msg, m)
	default:
	}
}

func newRegistered(t *testing.T, hub *Hub, id ClientIdentity, buf int) *Client {
	t.Helper()
	c := &Client{hub: hub, identity: id, send: make(chan []byte, buf)}
	require.NoError(t, hub.Register(c))
	return c
}

func topicSubscriberCount(h *Hub, topic string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.topicSubs[topic])
}

func TestHub_TopicDelivery(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	a := newRegistered(t, hub, ClientIdentity{}, 4)
	b := newRegistered(t, hub, ClientIdentity{}, 4)
	both := newRegistered(t, hub, ClientIdentity{}, 4)
	none := newRegistered(t, hub, ClientIdentity{}, 4)
	hub.Subscribe(a, "A")
	hub.Subscribe(b, "B")
	hub.Subscribe(both, "A")
	hub.Subscribe(both, "B")

	hub.BroadcastToTopic("A", []byte("to-a"))

	assert.Equal(t, "to-a", string(recv(t, a.send)))
	assert.Equal(t, "to-a", string(recv(t, both.send)))
	noMessage(t, b.send, "topic B client")
	noMessage(t, none.send, "unsubscribed client")

	hub.BroadcastToTopic("B", []byte("to-b"))
	assert.Equal(t, "to-b", string(recv(t, b.send)))
	assert.Equal(t, "to-b", string(recv(t, both.send)))
	noMessage(t, a.send, "topic A client")
}

func TestHub_TopicUnsubscribeStopsDelivery(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	c := newRegistered(t, hub, ClientIdentity{}, 4)
	hub.Subscribe(c, "A")
	hub.BroadcastToTopic("A", []byte("one"))
	assert.Equal(t, "one", string(recv(t, c.send)))

	hub.Unsubscribe(c, "A")
	hub.BroadcastToTopic("A", []byte("two"))
	noMessage(t, c.send, "after unsubscribe")
	assert.Zero(t, topicSubscriberCount(hub, "A"), "empty topic is removed")

	// Unsubscribing again or from an unknown topic is a no-op.
	assert.NotPanics(t, func() {
		hub.Unsubscribe(c, "A")
		hub.Unsubscribe(c, "never")
	})
}

func TestHub_TopicSubscribeRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		topic string
		want  int
	}{
		{name: "valid topic", topic: "items", want: 1},
		{name: "empty topic is ignored", topic: "", want: 0},
		{name: "topic at the length limit", topic: string(make([]byte, MaxTopicLength)), want: 1},
		{name: "topic over the length limit", topic: string(make([]byte, MaxTopicLength+1)), want: 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := NewHub()
			go hub.Run()
			defer hub.Shutdown()
			c := newRegistered(t, hub, ClientIdentity{}, 1)
			hub.Subscribe(c, tt.topic)
			assert.Equal(t, tt.want, topicSubscriberCount(hub, tt.topic))
		})
	}
}

func TestHub_TopicSubscribeIgnoresUnregisteredClient(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	never := &Client{hub: hub, send: make(chan []byte, 1)}
	hub.Subscribe(never, "A")
	assert.Zero(t, topicSubscriberCount(hub, "A"))

	removed := newRegistered(t, hub, ClientIdentity{UserID: "u1"}, 1)
	hub.DisconnectUser("u1")
	hub.Subscribe(removed, "A")
	assert.Zero(t, topicSubscriberCount(hub, "A"))
	// Would panic on a send to the closed channel if the subscription existed.
	assert.NotPanics(t, func() { hub.BroadcastToTopic("A", []byte("x")) })
}

func TestHub_TopicSubscriptionCap(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	c := newRegistered(t, hub, ClientIdentity{}, 1)
	for i := 0; i < MaxTopicsPerClient+5; i++ {
		hub.Subscribe(c, fmt.Sprintf("t%d", i))
	}
	hub.mu.RLock()
	got := len(hub.clientTopics[c])
	hub.mu.RUnlock()
	assert.Equal(t, MaxTopicsPerClient, got)

	// Re-subscribing to an existing topic is not counted twice, and a freed
	// slot can be used again.
	hub.Subscribe(c, "t0")
	hub.Unsubscribe(c, "t0")
	hub.Subscribe(c, "fresh")
	assert.Equal(t, 1, topicSubscriberCount(hub, "fresh"))
}

func TestHub_TopicBroadcastWithoutSubscribers(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	assert.NotPanics(t, func() { hub.BroadcastToTopic("nobody", []byte("x")) })
}

func TestHub_TopicSlowClientDroppedWithoutBlockingOthers(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	slow := newRegistered(t, hub, ClientIdentity{}, 1)
	fast := newRegistered(t, hub, ClientIdentity{}, 4)
	hub.Subscribe(slow, "A")
	hub.Subscribe(fast, "A")
	slow.send <- []byte("fill")

	done := make(chan struct{})
	go func() { hub.BroadcastToTopic("A", []byte("msg")); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("BroadcastToTopic blocked on a slow client")
	}

	assert.Equal(t, "msg", string(recv(t, fast.send)))
	assert.Equal(t, 1, hub.ClientCount(), "slow client is dropped")
	assert.Equal(t, 1, topicSubscriberCount(hub, "A"))
	assert.NotPanics(t, func() { hub.BroadcastToTopic("A", []byte("again")) })
}

func TestHub_BroadcastToUser(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	u1a := newRegistered(t, hub, ClientIdentity{UserID: "u1", TokenID: "a"}, 4)
	u1b := newRegistered(t, hub, ClientIdentity{UserID: "u1", TokenID: "b"}, 4)
	u2 := newRegistered(t, hub, ClientIdentity{UserID: "u2"}, 4)
	anon := newRegistered(t, hub, ClientIdentity{}, 4)

	hub.BroadcastToUser("u1", []byte("hello"))
	assert.Equal(t, "hello", string(recv(t, u1a.send)))
	assert.Equal(t, "hello", string(recv(t, u1b.send)))
	noMessage(t, u2.send, "other user")
	noMessage(t, anon.send, "anonymous client")

	// An empty user ID must not reach the clients without identity.
	hub.BroadcastToUser("", []byte("nobody"))
	noMessage(t, anon.send, "empty user ID")
	noMessage(t, u1a.send, "empty user ID")

	hub.BroadcastToUser("unknown", []byte("x"))
	noMessage(t, u1a.send, "unknown user")
}

func TestHub_BroadcastToUserSlowClientDropped(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	slow := newRegistered(t, hub, ClientIdentity{UserID: "u1"}, 1)
	fast := newRegistered(t, hub, ClientIdentity{UserID: "u1"}, 4)
	slow.send <- []byte("fill")

	done := make(chan struct{})
	go func() { hub.BroadcastToUser("u1", []byte("msg")); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("BroadcastToUser blocked on a slow client")
	}
	assert.Equal(t, "msg", string(recv(t, fast.send)))
	assert.Equal(t, 1, hub.ClientCount())
	assert.True(t, isClosed2(slow.send), "slow client send channel is closed")
}

// isClosed2 drains the queued message of a full channel and reports whether
// the channel is closed after it.
func isClosed2(ch chan []byte) bool {
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return true
			}
		default:
			return false
		}
	}
}

func TestHub_BroadcastDropsSlowClientTopics(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	slow := newRegistered(t, hub, ClientIdentity{}, 1)
	hub.Subscribe(slow, "A")
	slow.send <- []byte("fill")
	hub.Broadcast([]byte("overflow"))
	waitForClientCount(t, hub, 0)

	assert.Zero(t, topicSubscriberCount(hub, "A"))
	assert.NotPanics(t, func() { hub.BroadcastToTopic("A", []byte("x")) })
}

func TestHub_UnregisterRemovesTopics(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	c := newRegistered(t, hub, ClientIdentity{}, 1)
	hub.Subscribe(c, "A")
	hub.Unregister(c)
	waitForClientCount(t, hub, 0)
	assert.Zero(t, topicSubscriberCount(hub, "A"))
	hub.mu.RLock()
	_, has := hub.clientTopics[c]
	hub.mu.RUnlock()
	assert.False(t, has)
}

func TestHub_ShutdownClearsTopics(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	c := newRegistered(t, hub, ClientIdentity{}, 1)
	hub.Subscribe(c, "A")
	hub.Shutdown()
	waitForClientCount(t, hub, 0)
	assert.Zero(t, topicSubscriberCount(hub, "A"))
}

func TestHub_ImplementsSenders(t *testing.T) {
	t.Parallel()

	var (
		_ BroadcastSender = (*Hub)(nil)
		_ TopicSender     = (*Hub)(nil)
		_ UserSender      = (*Hub)(nil)
		_ ClientRevoker   = (*Hub)(nil)
	)
}

func TestHub_DisconnectClient(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	c := newRegistered(t, hub, ClientIdentity{UserID: "u1"}, 1)
	other := newRegistered(t, hub, ClientIdentity{UserID: "u1"}, 1)
	assert.True(t, hub.DisconnectClient(c))
	assert.False(t, hub.DisconnectClient(c), "already removed")
	assert.True(t, isClosed(c.send))
	assert.Equal(t, closeReasonRevoked, c.closeReason)
	assert.False(t, isClosed(other.send))

	var nilHub *Hub
	assert.False(t, nilHub.DisconnectClient(c))
	assert.False(t, hub.DisconnectClient(nil))
}

func TestClient_HandleInbound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msgs []string
		want int // subscribers of topic "A" afterwards
	}{
		{name: "subscribe", msgs: []string{`{"type":"subscribe","payload":{"topic":"A"}}`}, want: 1},
		{name: "subscribe then unsubscribe", msgs: []string{
			`{"type":"subscribe","payload":{"topic":"A"}}`,
			`{"type":"unsubscribe","payload":{"topic":"A"}}`,
		}, want: 0},
		{name: "unsubscribe without subscription", msgs: []string{`{"type":"unsubscribe","payload":{"topic":"A"}}`}, want: 0},
		{name: "missing topic", msgs: []string{`{"type":"subscribe","payload":{}}`}, want: 0},
		{name: "empty topic", msgs: []string{`{"type":"subscribe","payload":{"topic":""}}`}, want: 0},
		{name: "wrong payload type", msgs: []string{`{"type":"subscribe","payload":"A"}`}, want: 0},
		{name: "missing payload", msgs: []string{`{"type":"subscribe"}`}, want: 0},
		{name: "unknown type", msgs: []string{`{"type":"other","payload":{"topic":"A"}}`}, want: 0},
		{name: "invalid json", msgs: []string{`not json`}, want: 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := NewHub()
			go hub.Run()
			defer hub.Shutdown()
			c := newRegistered(t, hub, ClientIdentity{}, 1)
			for _, m := range tt.msgs {
				c.handleInbound([]byte(m))
			}
			assert.Equal(t, tt.want, topicSubscriberCount(hub, "A"))
		})
	}
}

// TestClient_SubscribeOverSocket runs the subscribe protocol end to end over a
// real WebSocket connection.
func TestClient_SubscribeOverSocket(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	srvConn, peer := newTestWSPair(t)
	_, err := NewClient(hub, srvConn)
	require.NoError(t, err)
	waitForClientCount(t, hub, 1)

	require.NoError(t, peer.WriteMessage(1, []byte(`{"type":"subscribe","payload":{"topic":"A"}}`)))
	require.Eventually(t, func() bool { return topicSubscriberCount(hub, "A") == 1 }, 2*time.Second, 5*time.Millisecond)

	hub.BroadcastToTopic("B", []byte("not-for-me"))
	hub.BroadcastToTopic("A", []byte("for-me"))
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, msg, err := peer.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, "for-me", string(msg))

	require.NoError(t, peer.WriteMessage(1, []byte(`{"type":"unsubscribe","payload":{"topic":"A"}}`)))
	require.Eventually(t, func() bool { return topicSubscriberCount(hub, "A") == 0 }, 2*time.Second, 5*time.Millisecond)
}
