package websocket

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitForClientCount polls hub.ClientCount until it equals want or the timeout expires.
func waitForClientCount(t *testing.T, hub *Hub, want int) {
	t.Helper()
	assert.Eventually(t, func() bool {
		return hub.ClientCount() == want
	}, time.Second, 5*time.Millisecond, "expected %d clients", want)
}

func TestHub_RegisterUnregister(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		registerCount int
		unregisterAll bool
		wantCount     int
	}{
		{
			name:          "register single client",
			registerCount: 1,
			unregisterAll: false,
			wantCount:     1,
		},
		{
			name:          "register multiple clients",
			registerCount: 3,
			unregisterAll: false,
			wantCount:     3,
		},
		{
			name:          "register then unregister all",
			registerCount: 2,
			unregisterAll: true,
			wantCount:     0,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hub := NewHub()
			go hub.Run()
			defer hub.Shutdown()

			clients := make([]*Client, tt.registerCount)
			for i := 0; i < tt.registerCount; i++ {
				c := &Client{
					hub:  hub,
					send: make(chan []byte, sendBufferSize),
				}
				err := hub.Register(c)
				require.NoError(t, err)
				clients[i] = c
			}

			waitForClientCount(t, hub, tt.registerCount)

			if tt.unregisterAll {
				for _, c := range clients {
					hub.Unregister(c)
				}
				waitForClientCount(t, hub, 0)
			}

			assert.Equal(t, tt.wantCount, hub.ClientCount())
		})
	}
}

func TestHub_Broadcast(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	c1 := &Client{hub: hub, send: make(chan []byte, sendBufferSize)}
	c2 := &Client{hub: hub, send: make(chan []byte, sendBufferSize)}
	err := hub.Register(c1)
	require.NoError(t, err)
	err = hub.Register(c2)
	require.NoError(t, err)
	waitForClientCount(t, hub, 2)

	msg := []byte(`{"type":"test","payload":"hello"}`)
	hub.Broadcast(msg)

	select {
	case received := <-c1.send:
		assert.Equal(t, msg, received)
	case <-time.After(time.Second):
		t.Fatal("client 1 did not receive broadcast in time")
	}

	select {
	case received := <-c2.send:
		assert.Equal(t, msg, received)
	case <-time.After(time.Second):
		t.Fatal("client 2 did not receive broadcast in time")
	}
}

func TestHub_Shutdown(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()

	c := &Client{hub: hub, send: make(chan []byte, sendBufferSize)}
	err := hub.Register(c)
	require.NoError(t, err)
	waitForClientCount(t, hub, 1)

	hub.Shutdown()
	waitForClientCount(t, hub, 0)

	assert.Equal(t, 0, hub.ClientCount())

	_, open := <-c.send
	assert.False(t, open, "client send channel should be closed after shutdown")
}

func TestHub_BroadcastDropsSlowClient(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	slow := &Client{hub: hub, send: make(chan []byte, 1)}
	err := hub.Register(slow)
	require.NoError(t, err)
	waitForClientCount(t, hub, 1)

	slow.send <- []byte("fill")

	hub.Broadcast([]byte("overflow"))

	waitForClientCount(t, hub, 0)
}

func TestHub_ImplementsBroadcastSender(t *testing.T) {
	t.Parallel()

	var _ BroadcastSender = (*Hub)(nil)
}

func TestHub_ShutdownIdempotent(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()

	// Calling Shutdown multiple times must not panic.
	hub.Shutdown()
	hub.Shutdown()
}

func TestHub_RegisterAfterShutdown(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	go hub.Run()
	hub.Shutdown()

	waitForClientCount(t, hub, 0)

	c := &Client{hub: hub, send: make(chan []byte, sendBufferSize)}
	err := hub.Register(c)
	assert.ErrorIs(t, err, ErrHubClosed)
}

func TestHub_BroadcastChannelFull(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	// Do NOT start hub.Run() — we test Broadcast() in isolation so the
	// Run loop cannot drain the channel concurrently.

	// Fill the broadcast channel to capacity.
	for i := 0; i < cap(hub.broadcast); i++ {
		hub.broadcast <- []byte("fill")
	}

	// The next Broadcast must not block — it should drop the message.
	done := make(chan struct{})
	go func() {
		hub.Broadcast([]byte("overflow"))
		close(done)
	}()

	select {
	case <-done:
		// Success — Broadcast returned without blocking.
	case <-time.After(time.Second):
		t.Fatal("Broadcast blocked when channel was full")
	}
}

func TestNewMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		msgType     string
		payload     interface{}
		wantType    string
		wantPayload string
		wantErr     bool
	}{
		{
			name:        "string payload",
			msgType:     "item.created",
			payload:     map[string]string{"name": "Widget"},
			wantType:    "item.created",
			wantPayload: `{"name":"Widget"}`,
		},
		{
			name:        "numeric payload",
			msgType:     "item.deleted",
			payload:     42,
			wantType:    "item.deleted",
			wantPayload: "42",
		},
		{
			name:        "nil payload",
			msgType:     "ping",
			payload:     nil,
			wantType:    "ping",
			wantPayload: "null",
		},
		{
			name:    "unmarshallable payload returns error",
			msgType: "bad",
			payload: math.Inf(1),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg, err := NewMessage(tt.msgType, tt.payload)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantType, msg.Type)
			assert.JSONEq(t, tt.wantPayload, string(msg.Payload))
		})
	}
}

func TestMessage_Bytes(t *testing.T) {
	t.Parallel()

	msg, err := NewMessage("item.updated", map[string]int{"id": 1})
	require.NoError(t, err)

	b, err := msg.Bytes()
	require.NoError(t, err)

	var parsed Message
	err = json.Unmarshal(b, &parsed)
	assert.NoError(t, err)
	assert.Equal(t, "item.updated", parsed.Type)
	assert.JSONEq(t, `{"id":1}`, string(parsed.Payload))
}
