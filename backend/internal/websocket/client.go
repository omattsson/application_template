package websocket

import (
	"log/slog"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// writeWait is the time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// pongWait is the time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// pingPeriod sends pings at this interval. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// maxMessageSize is the maximum message size allowed from peer.
	maxMessageSize = 512

	// sendBufferSize is the buffer size for the client send channel.
	sendBufferSize = 256
)

// Client is a middleman between the WebSocket connection and the hub.
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
}

// NewClient creates a new Client attached to the given hub and connection,
// registers it with the hub, and starts the read/write pumps.
// The caller should not interact with conn after calling NewClient.
// Returns an error if the hub has already been shut down.
func NewClient(hub *Hub, conn *websocket.Conn) (*Client, error) {
	client := &Client{
		hub:  hub,
		conn: conn,
		send: make(chan []byte, sendBufferSize),
	}
	if err := hub.Register(client); err != nil {
		conn.Close()
		return nil, err
	}
	go client.writePump()
	go client.readPump()
	return client, nil
}

// readPump pumps messages from the WebSocket connection to the hub.
// It runs in its own goroutine. When the connection is closed (or an
// error occurs), the client unregisters from the hub.
func (c *Client) readPump() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Panic in WebSocket readPump", "recover", r)
		}
		c.hub.Unregister(c)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	if err := c.conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		slog.Error("Failed to set read deadline", "error", err)
		return
	}
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Warn("WebSocket unexpected close", "error", err)
			}
			return
		}
		// Inbound messages from clients are currently ignored.
		// Future: route client messages through the hub or a handler.
	}
}

// writePump pumps messages from the hub to the WebSocket connection.
// It runs in its own goroutine. A ticker sends periodic pings to detect
// dead connections.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Panic in WebSocket writePump", "recover", r)
		}
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				slog.Error("Failed to set write deadline", "error", err)
				return
			}
			if !ok {
				// Hub closed the channel — send a close frame.
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			if _, err := w.Write(message); err != nil {
				return
			}

			// Drain queued messages into the same write frame for efficiency.
			n := len(c.send)
			for i := 0; i < n; i++ {
				if _, err := w.Write([]byte("\n")); err != nil {
					break
				}
				if _, err := w.Write(<-c.send); err != nil {
					break
				}
			}

			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				slog.Error("Failed to set write deadline for ping", "error", err)
				return
			}
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
