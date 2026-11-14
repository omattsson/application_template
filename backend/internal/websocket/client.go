package websocket

import (
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/gorilla/websocket"
)

// errChanClosed is a sentinel used internally when the hub closes a client's send channel.
var errChanClosed = errors.New("send channel closed")

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

// Close frame reasons for sockets that the server closes on purpose. They go
// out with close code 1008 (policy violation).
const (
	// closeReasonRevoked is the close reason when a logout or a user
	// revocation closes the socket (see Hub.DisconnectUser, DisconnectToken).
	closeReasonRevoked = "session revoked"
	// closeReasonExpired is the close reason when the access token that
	// opened the socket expires. The client reconnects with a fresh token.
	closeReasonExpired = "token expired"
	// closeReasonAPIKeyExpired is the close reason when the API key that
	// opened the socket expires.
	closeReasonAPIKeyExpired = "api key expired"
)

// ClientIdentity identifies the credential that opened a socket: an access
// token (TokenID, the jti) or an API key (APIKeyID). The hub uses it to close
// the sockets of a revoked user, token or API key and to address a user. The
// client uses ExpiresAt to close the socket when the token or the API key
// expires. All fields are empty when authentication is off.
type ClientIdentity struct {
	ExpiresAt time.Time // zero: the socket does not expire (for example an API key without expiry)
	IssuedAt  time.Time // when the credential was issued; zero when unknown
	UserID    string
	TokenID   string // jti claim; empty for an API key
	SessionID string // session family of the token; empty for an API key or a token without a session
	APIKeyID  string // ID of the API key; empty for an access token
	// Role is the role of the user when the socket opened (the token claim,
	// or the current role of an API key's owner). The topic authorizer uses
	// it. A role change ends the user's sessions and closes the socket, so
	// the role stays right for the life of the socket.
	Role string
}

// Client is a middleman between the WebSocket connection and the hub.
type Client struct {
	identity ClientIdentity
	hub      *Hub
	conn     *websocket.Conn
	send     chan []byte
	// closeReason is the reason of the close frame that writePump sends when
	// the hub closes send. The hub sets it under h.mu before it closes send.
	// writePump reads it only after the receive on the closed channel, so
	// the channel close orders the two accesses. Empty: no reason.
	closeReason string
	// subscribeCapWarned is set after the first subscribe over
	// MaxTopicsPerClient was logged at Warn; later ones log at Debug.
	// Guarded by hub.mu.
	subscribeCapWarned bool
}

// NewClient creates a new Client attached to the given hub and connection,
// registers it with the hub, and starts the read/write pumps.
// The caller should not interact with conn after calling NewClient.
// Returns an error if the hub has already been shut down.
func NewClient(hub *Hub, conn *websocket.Conn) (*Client, error) {
	return NewClientWithIdentity(hub, conn, ClientIdentity{})
}

// NewClientWithIdentity is NewClient for a socket opened by an authenticated
// caller. The hub can address the socket by user ID and close it by user ID,
// token ID or API key ID, and the socket closes when the credential expires.
func NewClientWithIdentity(hub *Hub, conn *websocket.Conn, identity ClientIdentity) (*Client, error) {
	client := &Client{
		identity: identity,
		hub:      hub,
		conn:     conn,
		send:     make(chan []byte, sendBufferSize),
	}
	if err := hub.Register(client); err != nil {
		if errors.Is(err, ErrTooManyConnections) {
			refuseSocket(conn)
		} else {
			_ = conn.Close() // best-effort close; the registration error is returned
		}
		return nil, err
	}
	go client.writePump()
	go client.readPump()
	return client, nil
}

// refuseWriteWait is the time allowed to write the close frame of a refused
// socket. The socket is new and its buffer empty, so the write is immediate
// unless the peer is gone; a short limit keeps the request goroutine from
// waiting on a dead peer.
const refuseWriteWait = 2 * time.Second

// refuseSocket tells the peer why (close code 1008, policy violation) and
// closes the connection. It starts no goroutine.
func refuseSocket(conn *websocket.Conn) {
	msg := websocket.FormatCloseMessage(websocket.ClosePolicyViolation, closeReasonTooManyConnections)
	_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(refuseWriteWait)) // best effort: the connection is closed next
	_ = conn.Close()
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
		_ = c.conn.Close() // best-effort close on teardown; the connection may already be closed
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
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Warn("WebSocket unexpected close", "error", err)
			}
			return
		}
		c.handleInbound(data)
	}
}

// writePump pumps messages from the hub to the WebSocket connection.
// It runs in its own goroutine. A ticker sends periodic pings to detect
// dead connections.
//
// When the client has a credential expiry, writePump sends a close frame and
// returns at that time. The client then reconnects with a fresh credential.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	// A nil channel blocks forever: no expiry timer for a zero ExpiresAt.
	var expired <-chan time.Time
	if !c.identity.ExpiresAt.IsZero() {
		timer := time.NewTimer(time.Until(c.identity.ExpiresAt))
		defer timer.Stop()
		expired = timer.C
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Panic in WebSocket writePump", "recover", r)
		}
		ticker.Stop()
		_ = c.conn.Close() // best-effort close on teardown; the connection may already be closed
	}()

	for {
		select {
		case <-expired:
			reason := closeReasonExpired
			if c.identity.APIKeyID != "" {
				reason = closeReasonAPIKeyExpired
			}
			c.writeClose(reason)
			return
		case message, ok := <-c.send:
			if err := c.handleSend(message, ok); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.writePing(); err != nil {
				return
			}
		}
	}
}

// handleSend processes a message (or channel close) from the hub.
func (c *Client) handleSend(message []byte, ok bool) error {
	if !ok {
		// Hub closed the channel — send a close frame.
		c.writeClose(c.closeReason)
		return errChanClosed
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		slog.Error("Failed to set write deadline", "error", err)
		return err
	}

	if err := c.writeMessage(message); err != nil {
		return err
	}

	// Drain queued messages, sending each as its own WS frame
	// so every frame contains a single valid JSON object.
	n := len(c.send)
	for i := 0; i < n; i++ {
		if err := c.writeMessage(<-c.send); err != nil {
			return err
		}
	}
	return nil
}

// writeClose sends a close frame. An empty reason sends an empty frame (hub
// shutdown, slow client). A reason sends code 1008 (policy violation) with the
// reason: the server closed the socket on purpose (revocation, expiry).
// Errors are ignored: the caller closes the connection next.
func (c *Client) writeClose(reason string) {
	data := []byte{}
	if reason != "" {
		data = websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason)
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	_ = c.conn.WriteMessage(websocket.CloseMessage, data)
}

// writePing sends a ping frame with the configured write deadline.
func (c *Client) writePing() error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		slog.Error("Failed to set write deadline for ping", "error", err)
		return err
	}
	return c.conn.WriteMessage(websocket.PingMessage, nil)
}

// writeMessage sends a single text message as one WebSocket frame.
func (c *Client) writeMessage(data []byte) error {
	w, err := c.conn.NextWriter(websocket.TextMessage)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return w.Close()
}

// inboundMsg is the envelope parsed from client-sent JSON.
type inboundMsg struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// topicPayload is the payload of the subscribe and unsubscribe messages.
type topicPayload struct {
	Topic string `json:"topic"`
}

// handleInbound routes inbound client messages. Malformed or unknown messages
// are ignored. It supports:
//   - subscribe: {"type":"subscribe","payload":{"topic":"..."}}
//   - unsubscribe: {"type":"unsubscribe","payload":{"topic":"..."}}
func (c *Client) handleInbound(data []byte) {
	var msg inboundMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	switch msg.Type {
	case MessageTypeSubscribe:
		var p topicPayload
		if json.Unmarshal(msg.Payload, &p) == nil && p.Topic != "" {
			c.hub.Subscribe(c, p.Topic)
		}
	case MessageTypeUnsubscribe:
		var p topicPayload
		if json.Unmarshal(msg.Payload, &p) == nil && p.Topic != "" {
			c.hub.Unsubscribe(c, p.Topic)
		}
	}
}
