// Package websocket provides WebSocket hub and client infrastructure for
// real-time communication between the backend and connected clients.
package websocket

import (
	"log/slog"
	"sync"
)

// BroadcastSender is implemented by any type that can broadcast messages
// to all connected WebSocket clients. Use this interface for decoupled
// dependency injection (e.g., handlers broadcast events without importing Hub).
type BroadcastSender interface {
	Broadcast(message []byte)
}

// Hub manages the set of active WebSocket clients and broadcasts messages
// to all of them. It is safe for concurrent use.
type Hub struct {
	// clients holds the set of registered clients.
	clients map[*Client]bool

	// broadcast receives messages to send to all clients.
	broadcast chan []byte

	// register receives clients requesting registration.
	register chan *Client

	// unregister receives clients requesting removal.
	unregister chan *Client

	// mu protects the clients map for reads outside the Run loop.
	mu sync.RWMutex

	// done signals the Run loop to stop.
	done chan struct{}
}

// NewHub creates a new Hub ready to accept clients.
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		done:       make(chan struct{}),
	}
}

// Run starts the hub's event loop. It should be launched as a goroutine.
// It processes register, unregister, and broadcast events until Shutdown is called.
func (h *Hub) Run() {
	for {
		select {
		case <-h.done:
			h.closeAllClients()
			return
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			slog.Info("WebSocket client registered", "clients", h.ClientCount())
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			h.mu.Unlock()
			slog.Info("WebSocket client unregistered", "clients", h.ClientCount())
		case message := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					// Client send buffer full — disconnect it.
					h.mu.RUnlock()
					h.mu.Lock()
					delete(h.clients, client)
					close(client.send)
					h.mu.Unlock()
					h.mu.RLock()
				}
			}
			h.mu.RUnlock()
		}
	}
}

// Broadcast sends a message to all connected clients.
// It is safe for concurrent use and implements BroadcastSender.
func (h *Hub) Broadcast(message []byte) {
	select {
	case h.broadcast <- message:
	default:
		slog.Warn("WebSocket broadcast channel full, message dropped")
	}
}

// Shutdown gracefully stops the hub's Run loop and closes all client connections.
func (h *Hub) Shutdown() {
	close(h.done)
}

// ClientCount returns the current number of connected clients.
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// closeAllClients removes all clients and closes their send channels.
func (h *Hub) closeAllClients() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for client := range h.clients {
		close(client.send)
		delete(h.clients, client)
	}
}
