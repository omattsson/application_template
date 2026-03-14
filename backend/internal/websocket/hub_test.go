package websocket

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

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
			for i := range tt.registerCount {
				c := &Client{
					hub:  hub,
					send: make(chan []byte, sendBufferSize),
				}
				hub.register <- c
				clients[i] = c
			}

			time.Sleep(50 * time.Millisecond)
			assert.Equal(t, tt.registerCount, hub.ClientCount())

			if tt.unregisterAll {
				for _, c := range clients {
					hub.unregister <- c
				}
				time.Sleep(50 * time.Millisecond)
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
	hub.register <- c1
	hub.register <- c2
	time.Sleep(50 * time.Millisecond)

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
	hub.register <- c
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 1, hub.ClientCount())

	hub.Shutdown()
	time.Sleep(50 * time.Millisecond)

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
	hub.register <- slow
	time.Sleep(50 * time.Millisecond)

	slow.send <- []byte("fill")

	hub.Broadcast([]byte("overflow"))
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 0, hub.ClientCount(), "slow client should have been dropped")
}

func TestHub_ImplementsBroadcastSender(t *testing.T) {
	t.Parallel()

	var _ BroadcastSender = (*Hub)(nil)
}

func TestNewMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		msgType     string
		payload     interface{}
		wantType    string
		wantPayload string
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
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg, err := NewMessage(tt.msgType, tt.payload)
			assert.NoError(t, err)
			assert.Equal(t, tt.wantType, msg.Type)
			assert.JSONEq(t, tt.wantPayload, string(msg.Payload))
		})
	}
}

func TestMessage_Bytes(t *testing.T) {
	t.Parallel()

	msg, err := NewMessage("item.updated", map[string]int{"id": 1})
	assert.NoError(t, err)

	b, err := msg.Bytes()
	assert.NoError(t, err)

	var parsed Message
	err = json.Unmarshal(b, &parsed)
	assert.NoError(t, err)
	assert.Equal(t, "item.updated", parsed.Type)
	assert.JSONEq(t, `{"id":1}`, string(parsed.Payload))
}
