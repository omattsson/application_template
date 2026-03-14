package websocket

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gorilla "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestWSPair creates a connected pair of WebSocket connections for testing.
//
// server is the connection to be owned by the Client under test;
// peer is the far-end connection used by the test to send/receive frames.
//
// Gorilla hijacks the underlying TCP connection on Upgrade, so the HTTP handler
// may return immediately without affecting the lifetime of the returned conns.
func newTestWSPair(t *testing.T) (server, peer *gorilla.Conn) {
	t.Helper()

	srvCh := make(chan *gorilla.Conn, 1)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := gorilla.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		c, err := u.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("newTestWSPair: upgrade: %v", err)
			return
		}
		srvCh <- c
		// Handler may return — gorilla has already hijacked the conn.
	}))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	p, _, err := gorilla.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err, "newTestWSPair: dial")

	srv := <-srvCh

	t.Cleanup(func() {
		srv.Close()
		p.Close()
	})
	return srv, p
}

// sendCloser wraps a send channel and provides idempotent close via sync.Once,
// preventing double-close panics when both a test and its cleanup close the channel.
type sendCloser struct {
	ch   chan []byte
	once sync.Once
}

func newSendCloser() *sendCloser {
	return &sendCloser{ch: make(chan []byte, sendBufferSize)}
}

func (s *sendCloser) close() {
	s.once.Do(func() { close(s.ch) })
}

// TestNewClient covers the NewClient constructor: success path and hub-closed path.
func TestNewClient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		closeHub  bool
		wantErr   error
		wantCount int
	}{
		{
			name:      "success - client registered with hub and pumps started",
			closeHub:  false,
			wantErr:   nil,
			wantCount: 1,
		},
		{
			name:      "hub already shut down - returns ErrHubClosed and closes conn",
			closeHub:  true,
			wantErr:   ErrHubClosed,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hub := NewHub()
			go hub.Run()

			if tt.closeHub {
				hub.Shutdown()
				// Wait for Run to process the shutdown before proceeding.
				waitForClientCount(t, hub, 0)
			} else {
				defer hub.Shutdown()
			}

			srvConn, _ := newTestWSPair(t)

			client, err := NewClient(hub, srvConn)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Nil(t, client)
				// NewClient must have closed the conn; verify it's unusable.
				assert.Error(t, srvConn.WriteMessage(gorilla.TextMessage, []byte("x")))
				return
			}

			require.NoError(t, err)
			require.NotNil(t, client)
			waitForClientCount(t, hub, tt.wantCount)
		})
	}
}

// TestClient_WriteMessage covers the writeMessage method directly.
func TestClient_WriteMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		msg         []byte
		closeBefore bool
		wantErr     bool
	}{
		{
			name:    "success - peer receives text frame",
			msg:     []byte(`{"type":"item.created","payload":{"id":42}}`),
			wantErr: false,
		},
		{
			name:        "closed conn - NextWriter returns error",
			msg:         []byte("data"),
			closeBefore: true,
			wantErr:     true,
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
			c := &Client{hub: hub, conn: srvConn, send: make(chan []byte, sendBufferSize)}

			if tt.closeBefore {
				require.NoError(t, srvConn.Close())
			}

			err := c.writeMessage(tt.msg)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)

			require.NoError(t, peerConn.SetReadDeadline(time.Now().Add(2*time.Second)))
			_, data, readErr := peerConn.ReadMessage()
			require.NoError(t, readErr)
			assert.Equal(t, tt.msg, data)
		})
	}
}

// TestClient_WritePing covers the writePing method.
func TestClient_WritePing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		closeBefore bool
		wantErr     bool
	}{
		{
			name:    "success - ping frame sent and SetWriteDeadline applied",
			wantErr: false,
		},
		{
			name:        "closed conn - SetWriteDeadline fails, error returned",
			closeBefore: true,
			wantErr:     true,
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
			c := &Client{hub: hub, conn: srvConn, send: make(chan []byte, sendBufferSize)}

			if tt.closeBefore {
				require.NoError(t, srvConn.Close())
			}

			err := c.writePing()
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)

			// Observe the ping arriving at the peer by installing a custom PingHandler,
			// then driving the peer's read loop so the handler fires.
			pingCh := make(chan struct{}, 1)
			peerConn.SetPingHandler(func(string) error {
				pingCh <- struct{}{}
				return nil
			})
			go func() {
				_ = peerConn.SetReadDeadline(time.Now().Add(2 * time.Second))
				_, _, _ = peerConn.ReadMessage()
			}()

			select {
			case <-pingCh:
				// Ping arrived at peer.
			case <-time.After(2 * time.Second):
				t.Error("peer did not receive ping within timeout")
			}
		})
	}
}

// TestClient_HandleSend covers handleSend: ok=false, ok=true single, queue drain, and write errors.
func TestClient_HandleSend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		msg         []byte
		ok          bool
		queuedMsgs  [][]byte
		closeBefore bool
		wantErr     error // exact sentinel via errors.Is
		wantAnyErr  bool  // just any error (for write failures)
		wantReceive [][]byte
	}{
		{
			name:    "ok=false - sends close frame and returns errChanClosed",
			msg:     nil,
			ok:      false,
			wantErr: errChanClosed,
		},
		{
			name:        "ok=true - single message delivered to peer",
			msg:         []byte(`{"type":"x","payload":1}`),
			ok:          true,
			wantReceive: [][]byte{[]byte(`{"type":"x","payload":1}`)},
		},
		{
			name:        "ok=true - queued messages drained in order",
			msg:         []byte(`first`),
			ok:          true,
			queuedMsgs:  [][]byte{[]byte(`second`), []byte(`third`)},
			wantReceive: [][]byte{[]byte(`first`), []byte(`second`), []byte(`third`)},
		},
		{
			// Closed conn causes SetWriteDeadline to fail → error returned early.
			name:        "ok=true - closed conn returns write deadline error",
			msg:         []byte(`data`),
			ok:          true,
			closeBefore: true,
			wantAnyErr:  true,
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
			c := &Client{hub: hub, conn: srvConn, send: make(chan []byte, sendBufferSize)}

			// Pre-fill the send channel with queued messages.
			for _, m := range tt.queuedMsgs {
				c.send <- m
			}

			if tt.closeBefore {
				require.NoError(t, srvConn.Close())
			}

			err := c.handleSend(tt.msg, tt.ok)

			switch {
			case tt.wantErr != nil:
				assert.ErrorIs(t, err, tt.wantErr)
			case tt.wantAnyErr:
				assert.Error(t, err)
			default:
				require.NoError(t, err)
			}

			// Verify the peer received every expected message in order.
			for _, want := range tt.wantReceive {
				require.NoError(t, peerConn.SetReadDeadline(time.Now().Add(2*time.Second)))
				_, data, readErr := peerConn.ReadMessage()
				require.NoError(t, readErr)
				assert.Equal(t, want, data)
			}
		})
	}
}

// TestClient_ReadPump starts readPump as a goroutine and verifies it unregisters
// the client from the hub after various peer-close scenarios.
func TestClient_ReadPump(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		action func(peerConn *gorilla.Conn)
	}{
		{
			// Peer sends close frame with NormalClosure — IsUnexpectedCloseError returns false.
			name: "normal close frame causes unregister",
			action: func(p *gorilla.Conn) {
				_ = p.WriteControl(
					gorilla.CloseMessage,
					gorilla.FormatCloseMessage(gorilla.CloseNormalClosure, "bye"),
					time.Now().Add(time.Second),
				)
			},
		},
		{
			// Peer drops the TCP connection without a close frame — IsUnexpectedCloseError may
			// return false (non-CloseError) but readPump still exits and unregisters.
			name: "unexpected TCP drop causes unregister",
			action: func(p *gorilla.Conn) {
				p.Close()
			},
		},
		{
			// Peer sends a close frame with a code outside [NormalClosure, GoingAway],
			// so IsUnexpectedCloseError returns true and slog.Warn is emitted.
			name: "unexpected close code triggers warn log and unregister",
			action: func(p *gorilla.Conn) {
				_ = p.WriteControl(
					gorilla.CloseMessage,
					gorilla.FormatCloseMessage(gorilla.CloseProtocolError, "bad"),
					time.Now().Add(time.Second),
				)
			},
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
			c := &Client{hub: hub, conn: srvConn, send: make(chan []byte, sendBufferSize)}

			// Register the client manually (without starting writePump).
			require.NoError(t, hub.Register(c))
			waitForClientCount(t, hub, 1)

			go c.readPump()

			// Trigger the close scenario.
			tt.action(peerConn)

			// readPump must exit and call hub.Unregister, dropping the count to 0.
			waitForClientCount(t, hub, 0)
		})
	}
}

// TestClient_WritePump starts writePump as a goroutine and verifies it forwards
// hub messages to the peer and exits cleanly when the send channel is closed.
func TestClient_WritePump(t *testing.T) {
	t.Parallel()

	t.Run("single message forwarded to peer", func(t *testing.T) {
		t.Parallel()

		hub := NewHub()
		go hub.Run()
		defer hub.Shutdown()

		srvConn, peerConn := newTestWSPair(t)
		sc := newSendCloser()
		c := &Client{hub: hub, conn: srvConn, send: sc.ch}
		t.Cleanup(sc.close) // ensure writePump goroutine is eventually terminated

		go c.writePump()

		msg := []byte(`{"type":"item.updated","payload":{"id":7}}`)
		sc.ch <- msg

		require.NoError(t, peerConn.SetReadDeadline(time.Now().Add(2*time.Second)))
		_, received, err := peerConn.ReadMessage()
		require.NoError(t, err)
		assert.Equal(t, msg, received)
	})

	t.Run("multiple messages delivered in order", func(t *testing.T) {
		t.Parallel()

		hub := NewHub()
		go hub.Run()
		defer hub.Shutdown()

		srvConn, peerConn := newTestWSPair(t)
		sc := newSendCloser()
		c := &Client{hub: hub, conn: srvConn, send: sc.ch}
		t.Cleanup(sc.close)

		go c.writePump()

		msgs := [][]byte{
			[]byte(`{"seq":1}`),
			[]byte(`{"seq":2}`),
			[]byte(`{"seq":3}`),
		}
		for _, m := range msgs {
			sc.ch <- m
		}

		for _, want := range msgs {
			require.NoError(t, peerConn.SetReadDeadline(time.Now().Add(2*time.Second)))
			_, got, err := peerConn.ReadMessage()
			require.NoError(t, err)
			assert.Equal(t, want, got)
		}
	})

	t.Run("closed send channel sends close frame and exits", func(t *testing.T) {
		t.Parallel()

		hub := NewHub()
		go hub.Run()
		defer hub.Shutdown()

		srvConn, peerConn := newTestWSPair(t)
		sc := newSendCloser()
		c := &Client{hub: hub, conn: srvConn, send: sc.ch}

		go c.writePump()

		// Closing the channel causes writePump to call handleSend(nil, false),
		// which writes a WS CloseMessage and returns errChanClosed → writePump exits
		// and its defer calls conn.Close().
		sc.close()

		require.NoError(t, peerConn.SetReadDeadline(time.Now().Add(2*time.Second)))
		_, _, err := peerConn.ReadMessage()
		assert.Error(t, err, "peer should observe close after send channel is closed")
	})
}
