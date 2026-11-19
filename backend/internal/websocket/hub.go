// Package websocket provides WebSocket hub and client infrastructure for
// real-time communication between the backend and connected clients.
package websocket

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"backend/internal/logutil"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const (
	// broadcastBufferSize is the capacity of the Hub's broadcast channel.
	// Messages exceeding this buffer are dropped with a warning log.
	broadcastBufferSize = 256

	// MaxTopicLength is the longest topic name (in bytes) that Subscribe
	// accepts. A longer topic is ignored.
	MaxTopicLength = 128

	// MaxTopicsPerClient is the most topics one client can be subscribed to.
	// Further subscriptions are ignored, so a client cannot grow the hub's
	// subscription maps without bound.
	MaxTopicsPerClient = 64

	// DefaultMaxConnectionsPerIdentity is the default for the most /ws
	// sockets that one user, and one API key, may have open at once
	// (WS_MAX_CONNECTIONS_PER_USER). A socket over the cap is refused.
	DefaultMaxConnectionsPerIdentity = 16

	// closeReasonTooManyConnections is the close reason (code 1008) of a socket
	// that the connection cap refused.
	closeReasonTooManyConnections = "too many connections"

	// connectionCapLogInterval is the time between two Warn lines about
	// refused sockets.
	connectionCapLogInterval = time.Minute
)

// ErrHubClosed is returned when attempting to register a client on a shut-down hub.
var ErrHubClosed = errHubClosed{}

type errHubClosed struct{}

func (errHubClosed) Error() string { return "hub is closed" }

// ErrTooManyConnections is returned by Register when the user or the API key
// of the client already has the most sockets that the hub allows
// (SetMaxConnectionsPerIdentity).
var ErrTooManyConnections = errTooManyConnections{}

type errTooManyConnections struct{}

func (errTooManyConnections) Error() string {
	return "too many WebSocket connections for this identity"
}

// BroadcastSender is implemented by any type that can broadcast messages
// to all connected WebSocket clients. Use this interface for decoupled
// dependency injection (e.g., handlers broadcast events without importing Hub).
type BroadcastSender interface {
	Broadcast(message []byte)
}

// TopicSender sends a message only to the clients subscribed to a topic. Use
// it for high-volume streams that most clients do not watch.
type TopicSender interface {
	BroadcastToTopic(topic string, message []byte)
}

// UserSender sends a message only to the clients of one user (for example an
// in-app notification).
type UserSender interface {
	BroadcastToUser(userID string, message []byte)
}

// ClientRevoker closes open WebSocket connections after a revocation. Auth
// and user handlers depend on it, not on *Hub. Each method returns the number
// of closed connections. *Hub implements it; a nil *Hub is safe and closes
// nothing.
//
// The hub only knows the connections of this process. Other replicas close
// their sockets at the next revalidation (see StartRevalidation) or at token
// expiry.
type ClientRevoker interface {
	// DisconnectUser closes all connections of the user (user deleted,
	// disabled, password reset, logout of all sessions).
	DisconnectUser(userID string) int
	// DisconnectToken closes all connections opened with the access token
	// that has this jti (logout).
	DisconnectToken(tokenID string) int
	// DisconnectAPIKey closes all connections opened with the API key that
	// has this ID (key deleted).
	DisconnectAPIKey(keyID string) int
}

var (
	_ BroadcastSender = (*Hub)(nil)
	_ TopicSender     = (*Hub)(nil)
	_ UserSender      = (*Hub)(nil)
	_ ClientRevoker   = (*Hub)(nil)
)

// Hub manages the set of active WebSocket clients and broadcasts messages
// to all of them, to the subscribers of a topic, or to the sockets of one
// user. It is safe for concurrent use.
type Hub struct {
	// clients holds the set of registered clients.
	clients map[*Client]bool

	// topicSubs maps a topic to the set of clients subscribed to it.
	topicSubs map[string]map[*Client]bool

	// clientTopics is the reverse index of topicSubs: the topics of a client.
	// It makes removal and the per-client cap cheap.
	clientTopics map[*Client]map[string]struct{}

	// broadcast receives messages to send to all clients.
	broadcast chan []byte

	// unregister receives clients requesting removal.
	unregister chan *Client

	// mu protects clients, topicSubs, clientTopics and revocables.
	mu sync.RWMutex

	// done signals the Run loop to stop.
	done chan struct{}

	// shutdownOnce ensures Shutdown is idempotent and safe to call concurrently.
	shutdownOnce sync.Once

	// revocables holds the registered revocable connections. They are not
	// clients: no broadcast, no ClientCount.
	revocables map[Revocable]struct{}
	// accessChecker is the optional access re-check of the revocables.
	accessChecker atomic.Pointer[RevocableAccessChecker]
	// topicAuthorizer decides which topics a client may subscribe to. Nil:
	// every topic (authentication off).
	topicAuthorizer atomic.Pointer[TopicAuthorizer]

	// revalidateOnce makes StartRevalidation start at most one loop.
	revalidateOnce sync.Once
	// revalidateDone is closed when the revalidation loop exits (tests).
	revalidateDone chan struct{}

	// maxPerIdentity is the most sockets of one user and of one API key
	// (SetMaxConnectionsPerIdentity); 0 or less: no cap.
	maxPerIdentity atomic.Int32
	// identityConns counts the registered clients per identity key (see
	// identityKeys). An entry exists only while its count is above zero.
	// Protected by mu, like clients.
	identityConns map[string]int
	// capLog limits the log line of a refused socket: a client over the cap
	// that keeps reconnecting would flood the log.
	capLog *logutil.Limiter

	// dropLog limits the log line of a dropped broadcast: with the channel
	// full, every Broadcast drops its message. dropped counts every drop.
	dropLog *logutil.Limiter
	dropped metric.Int64Counter
}

// NewHub creates a new Hub ready to accept clients.
func NewHub() *Hub {
	// The counter binds to the MeterProvider of the moment: the bootstrap
	// creates the hub after telemetry.Init. A failed creation leaves it nil.
	dropped, err := otel.Meter("websocket").Int64Counter("ws.broadcast.dropped.total",
		metric.WithDescription("Broadcast messages dropped because the hub's broadcast channel was full."),
		metric.WithUnit("{message}"))
	if err != nil {
		otel.Handle(err)
	}

	h := &Hub{
		capLog:         logutil.NewLimiter(connectionCapLogInterval),
		identityConns:  make(map[string]int),
		dropLog:        logutil.NewLimiter(time.Minute),
		dropped:        dropped,
		clients:        make(map[*Client]bool),
		topicSubs:      make(map[string]map[*Client]bool),
		clientTopics:   make(map[*Client]map[string]struct{}),
		revocables:     make(map[Revocable]struct{}),
		broadcast:      make(chan []byte, broadcastBufferSize),
		unregister:     make(chan *Client),
		done:           make(chan struct{}),
		revalidateDone: make(chan struct{}),
	}
	h.maxPerIdentity.Store(DefaultMaxConnectionsPerIdentity)
	return h
}

// SetMaxConnectionsPerIdentity sets the most sockets that one user (UserID)
// and one API key (APIKeyID) may have open at once. A socket over the cap is
// refused by Register with ErrTooManyConnections. A socket without identity
// (authentication off) is not counted: the identity comes from the verified
// credential only, never from the request, so a client cannot choose one.
// n <= 0 turns the cap off. Call it before the traffic starts.
func (h *Hub) SetMaxConnectionsPerIdentity(n int) {
	h.maxPerIdentity.Store(int32(max(n, 0))) // #nosec G115 -- a connection cap, far below int32
}

// identityKeys returns the counters of the identity: one for the user and one
// for the API key, "" for a part that is empty.
func identityKeys(id ClientIdentity) [2]string {
	var keys [2]string
	if id.UserID != "" {
		keys[0] = "user:" + id.UserID
	}
	if id.APIKeyID != "" {
		keys[1] = "key:" + id.APIKeyID
	}
	return keys
}

// Run starts the hub's event loop. It should be launched as a goroutine.
// It processes unregister and broadcast events until Shutdown is called.
func (h *Hub) Run() {
	for {
		select {
		case <-h.done:
			h.closeAllClients()
			return
		case client := <-h.unregister:
			h.mu.Lock()
			h.removeClientLocked(client, "")
			h.mu.Unlock()
			slog.Info("WebSocket client unregistered", "clients", h.ClientCount())
		case message := <-h.broadcast:
			h.mu.RLock()
			var slow []*Client
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					slow = append(slow, client)
				}
			}
			h.mu.RUnlock()
			h.dropSlow(slow)
		}
	}
}

// Broadcast sends a message to all connected clients.
// It is safe for concurrent use and implements BroadcastSender.
func (h *Hub) Broadcast(message []byte) {
	select {
	case h.broadcast <- message:
	default:
		if h.dropped != nil {
			h.dropped.Add(context.Background(), 1)
		}
		// One Warn per interval: a full channel drops every message, and a
		// line per message would flood the log.
		if ok, suppressed := h.dropLog.Allow(); ok {
			if suppressed > 0 {
				slog.Warn("WebSocket broadcast channel full, message dropped", "suppressed", suppressed)
			} else {
				slog.Warn("WebSocket broadcast channel full, message dropped")
			}
		} else {
			slog.Debug("WebSocket broadcast channel full, message dropped")
		}
	}
}

// BroadcastBacklog returns how full the busiest queue of a broadcast is: the
// highest number of waiting messages among the broadcast queue and the send
// queue of every client, and their capacity (the two have the same size). A
// message that does not fit the broadcast queue is dropped, and a client whose
// send queue is full is disconnected as too slow, so a sender that feeds the
// hub in bursts (the outbox poller) waits while the backlog is half the
// capacity.
func (h *Hub) BroadcastBacklog() (pending, capacity int) {
	pending, capacity = len(h.broadcast), cap(h.broadcast)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for client := range h.clients {
		if n := len(client.send); n > pending {
			pending = n
		}
	}
	return pending, capacity
}

// Shutdown gracefully stops the hub's Run loop and closes all client connections.
// It is safe to call multiple times and concurrently.
func (h *Hub) Shutdown() {
	h.shutdownOnce.Do(func() {
		close(h.done)
	})
}

// Register registers a client with the hub. The client is in the client set
// when Register returns, so a Subscribe or a revoke right after it sees the
// client. It returns ErrHubClosed if the hub has been shut down, and
// ErrTooManyConnections if the user or the API key of the client already has
// the most sockets that SetMaxConnectionsPerIdentity allows. The done check
// and the cap check run under h.mu with the registration: closeAllClients
// also takes h.mu after done is closed, so a client is either refused or
// closed by the shutdown, and concurrent registrations of one identity never
// exceed the cap.
func (h *Hub) Register(c *Client) error {
	h.mu.Lock()
	select {
	case <-h.done:
		h.mu.Unlock()
		return ErrHubClosed
	default:
	}
	keys := identityKeys(c.identity)
	if limit := int(h.maxPerIdentity.Load()); limit > 0 {
		for _, key := range keys {
			if key != "" && h.identityConns[key] >= limit {
				h.mu.Unlock()
				h.logRefused(c.identity, limit)
				return ErrTooManyConnections
			}
		}
	}
	h.clients[c] = true
	for _, key := range keys {
		if key != "" {
			h.identityConns[key]++
		}
	}
	count := len(h.clients)
	h.mu.Unlock()
	slog.Info("WebSocket client registered", "clients", count)
	return nil
}

// logRefused logs a socket that the cap refused: one Warn per interval (with
// the number of lines left out), the others at Debug. Only the user ID and the
// key ID are logged, never a token.
func (h *Hub) logRefused(id ClientIdentity, limit int) {
	level := slog.LevelDebug
	attrs := []any{"user_id", id.UserID, "api_key_id", id.APIKeyID, "limit", limit}
	if ok, suppressed := h.capLog.Allow(); ok {
		level = slog.LevelWarn
		if suppressed > 0 {
			attrs = append(attrs, "suppressed", suppressed)
		}
	}
	slog.Log(context.Background(), level, "WebSocket connection limit reached, socket refused", attrs...)
}

// Unregister safely requests client removal from the hub. If the hub has
// already been shut down the call is a no-op, preventing goroutine leaks.
func (h *Hub) Unregister(c *Client) {
	select {
	case h.unregister <- c:
	case <-h.done:
	}
}

// ClientCount returns the current number of connected clients.
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// TopicAuthorizer reports whether the client with this identity may
// subscribe to the topic. It runs for every subscribe frame, outside the hub
// lock: keep it cheap and free of I/O. It must deny what it does not know
// (deny by default), so a new topic is private until a rule allows it.
type TopicAuthorizer func(identity ClientIdentity, topic string) bool

// SetTopicAuthorizer sets the check of every subscription. Call it before the
// traffic starts. A nil authorizer allows every topic (the behaviour with
// authentication off). A refused subscription is ignored like the other
// ignored subscriptions (logged at debug level).
func (h *Hub) SetTopicAuthorizer(authorize TopicAuthorizer) {
	if authorize == nil {
		h.topicAuthorizer.Store(nil)
		return
	}
	h.topicAuthorizer.Store(&authorize)
}

// topicAllowed runs the topic authorizer, if any.
func (h *Hub) topicAllowed(c *Client, topic string) bool {
	authorize := h.topicAuthorizer.Load()
	if authorize == nil {
		return true
	}
	if (*authorize)(c.identity, topic) {
		return true
	}
	slog.Debug("WebSocket subscription refused", "user_id", c.identity.UserID, "topic_len", len(topic))
	return false
}

// TopicSubscriberCount returns the number of clients subscribed to the
// topic. A nil hub has none.
func (h *Hub) TopicSubscriberCount(topic string) int {
	if h == nil {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.topicSubs[topic])
}

// Subscribe registers a client's interest in the messages of a topic. An
// empty topic, a topic longer than MaxTopicLength, a topic that the topic
// authorizer refuses (SetTopicAuthorizer), and a subscription beyond
// MaxTopicsPerClient are ignored. A client that is not registered (never
// registered, or already removed by a revoke, a slow-client drop or an
// unregister) is ignored too: its send channel may be closed, and a
// subscription would make BroadcastToTopic send on it.
func (h *Hub) Subscribe(c *Client, topic string) {
	if topic == "" || len(topic) > MaxTopicLength {
		return
	}
	if !h.topicAllowed(c, topic) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.clients[c] {
		return
	}
	topics := h.clientTopics[c]
	if _, ok := topics[topic]; !ok && len(topics) >= MaxTopicsPerClient {
		// /ws is outside the rate limiter: Warn once per client, so a client
		// that keeps sending subscribe frames cannot flood the log.
		level := slog.LevelDebug
		if !c.subscribeCapWarned {
			c.subscribeCapWarned = true
			level = slog.LevelWarn
		}
		slog.Log(context.Background(), level, "WebSocket subscription limit reached, subscription ignored", "limit", MaxTopicsPerClient)
		return
	}
	if topics == nil {
		topics = make(map[string]struct{})
		h.clientTopics[c] = topics
	}
	topics[topic] = struct{}{}
	subs, ok := h.topicSubs[topic]
	if !ok {
		subs = make(map[*Client]bool)
		h.topicSubs[topic] = subs
	}
	subs[c] = true
}

// Unsubscribe removes a client's interest in a topic.
func (h *Hub) Unsubscribe(c *Client, topic string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if subs, ok := h.topicSubs[topic]; ok {
		delete(subs, c)
		if len(subs) == 0 {
			delete(h.topicSubs, topic)
		}
	}
	if topics, ok := h.clientTopics[c]; ok {
		delete(topics, topic)
		if len(topics) == 0 {
			delete(h.clientTopics, c)
		}
	}
}

// BroadcastToTopic sends a message only to the clients subscribed to the
// topic. With no subscribers nothing is sent. A subscriber with a full send
// buffer is dropped without blocking the others. It implements TopicSender.
func (h *Hub) BroadcastToTopic(topic string, message []byte) {
	h.mu.RLock()
	subs := h.topicSubs[topic]
	if len(subs) == 0 {
		h.mu.RUnlock()
		return
	}
	var slow []*Client
	for client := range subs {
		// Send only to registered clients. A removed client has a closed
		// send channel; a send on it would panic.
		if !h.clients[client] {
			continue
		}
		select {
		case client.send <- message:
		default:
			slow = append(slow, client)
		}
	}
	h.mu.RUnlock()
	h.dropSlow(slow)
}

// BroadcastToUser sends a message only to the clients of the user. An empty
// user ID sends nothing, so the sockets without an identity never get a user
// message. A client with a full send buffer is dropped without blocking the
// others. It implements UserSender.
func (h *Hub) BroadcastToUser(userID string, message []byte) {
	if userID == "" {
		return
	}
	h.mu.RLock()
	var slow []*Client
	for client := range h.clients {
		if client.identity.UserID != userID {
			continue
		}
		select {
		case client.send <- message:
		default:
			slow = append(slow, client)
		}
	}
	h.mu.RUnlock()
	h.dropSlow(slow)
}

// dropSlow removes the clients whose send buffer was full.
func (h *Hub) dropSlow(slow []*Client) {
	if len(slow) == 0 {
		return
	}
	h.mu.Lock()
	for _, client := range slow {
		h.removeClientLocked(client, "")
	}
	h.mu.Unlock()
}

// DisconnectUser closes all connections of the user. It implements
// ClientRevoker. An empty user ID or a nil hub closes nothing.
func (h *Hub) DisconnectUser(userID string) int {
	if h == nil || userID == "" {
		return 0
	}
	return h.disconnectIdentities(func(id ClientIdentity) bool { return id.UserID == userID })
}

// DisconnectToken closes all connections opened with the access token that
// has this jti. It implements ClientRevoker. An empty token ID or a nil hub
// closes nothing.
func (h *Hub) DisconnectToken(tokenID string) int {
	if h == nil || tokenID == "" {
		return 0
	}
	return h.disconnectIdentities(func(id ClientIdentity) bool { return id.TokenID == tokenID })
}

// DisconnectAPIKey closes all connections opened with the API key that has
// this ID. It implements ClientRevoker. An empty key ID or a nil hub closes
// nothing.
func (h *Hub) DisconnectAPIKey(keyID string) int {
	if h == nil || keyID == "" {
		return 0
	}
	return h.disconnectIdentities(func(id ClientIdentity) bool { return id.APIKeyID == keyID })
}

// disconnectIdentities closes the clients (send channel closed, "session
// revoked" close frame by the write pump) and the revocables (Revoke 1008,
// outside the lock) whose identity matches. The read pump of a closed client
// unregisters, which is a no-op because the client is already removed. It
// returns the number of closed connections.
func (h *Hub) disconnectIdentities(match func(ClientIdentity) bool) int {
	h.mu.Lock()
	closed := 0
	for client := range h.clients {
		if match(client.identity) {
			h.removeClientLocked(client, closeReasonRevoked)
			closed++
		}
	}
	taken := h.takeRevocablesLocked(func(r Revocable) bool { return match(r.Identity()) })
	h.mu.Unlock()
	revokeAll(taken, CloseCodePolicyViolation, closeReasonRevoked)
	return closed + len(taken)
}

// removeClientLocked removes a registered client from all maps and closes its
// send channel. reason is the close frame reason (empty: no reason). It does
// nothing for a client that is not registered, so a send channel is never
// closed twice. Caller must hold h.mu (write lock).
func (h *Hub) removeClientLocked(c *Client, reason string) {
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	h.releaseIdentityLocked(c)
	c.closeReason = reason
	close(c.send)
	h.removeClientTopicsLocked(c)
}

// releaseIdentityLocked counts a removed client off its identity counters and
// drops a counter that reaches zero. Caller holds h.mu (write lock).
func (h *Hub) releaseIdentityLocked(c *Client) {
	for _, key := range identityKeys(c.identity) {
		if key == "" {
			continue
		}
		if n := h.identityConns[key]; n > 1 {
			h.identityConns[key] = n - 1
		} else {
			delete(h.identityConns, key)
		}
	}
}

// removeClientTopicsLocked removes a client from all topic subscriptions.
// Caller must hold h.mu (write lock).
func (h *Hub) removeClientTopicsLocked(c *Client) {
	for topic := range h.clientTopics[c] {
		if subs, ok := h.topicSubs[topic]; ok {
			delete(subs, c)
			if len(subs) == 0 {
				delete(h.topicSubs, topic)
			}
		}
	}
	delete(h.clientTopics, c)
}

// closeAllClients removes all clients and closes their send channels.
func (h *Hub) closeAllClients() {
	h.mu.Lock()
	revocables := h.takeRevocablesLocked(func(Revocable) bool { return true })
	defer revokeAll(revocables, CloseCodeGoingAway, CloseReasonShutdown) // runs after the unlock
	defer h.mu.Unlock()
	for client := range h.clients {
		close(client.send)
		delete(h.clients, client)
	}
	h.topicSubs = make(map[string]map[*Client]bool)
	h.clientTopics = make(map[*Client]map[string]struct{})
	h.identityConns = make(map[string]int)
}

// DisconnectClient closes one client, for example when a revocation check
// after the upgrade finds the token revoked. It reports whether the client
// was registered. A nil hub or client closes nothing.
func (h *Hub) DisconnectClient(c *Client) bool {
	if h == nil || c == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.clients[c] {
		return false
	}
	h.removeClientLocked(c, closeReasonRevoked)
	return true
}
