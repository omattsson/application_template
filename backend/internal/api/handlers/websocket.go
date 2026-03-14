package handlers

import (
	"log/slog"
	"net/http"
	"strings"

	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	gorilla "github.com/gorilla/websocket"
)

// WebSocketHandler handles WebSocket connection upgrades.
// It is a separate struct from Handler because it depends on *websocket.Hub
// rather than models.Repository.
type WebSocketHandler struct {
	hub            *websocket.Hub
	allowedOrigins string
}

// NewWebSocketHandler creates a new WebSocketHandler with the given hub and allowed origins config.
func NewWebSocketHandler(hub *websocket.Hub, allowedOrigins string) *WebSocketHandler {
	return &WebSocketHandler{
		hub:            hub,
		allowedOrigins: allowedOrigins,
	}
}

// HandleWebSocket godoc
// @Summary Open a WebSocket connection
// @Description Upgrades the HTTP connection to a WebSocket for real-time events.
// @Tags websocket
// @Success 101 "Switching Protocols"
// @Failure 400 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Router /ws [get]
func (h *WebSocketHandler) HandleWebSocket(c *gin.Context) {
	upgrader := gorilla.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     h.checkOrigin,
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		slog.Error("WebSocket upgrade failed", "error", err)
		return
	}

	if _, err := websocket.NewClient(h.hub, conn); err != nil {
		slog.Error("WebSocket client creation failed", "error", err)
		return
	}
}

// checkOrigin validates the request origin against the configured allowed origins.
func (h *WebSocketHandler) checkOrigin(r *http.Request) bool {
	if h.allowedOrigins == "" || h.allowedOrigins == "*" {
		return true
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}

	for _, allowed := range strings.Split(h.allowedOrigins, ",") {
		if strings.TrimSpace(allowed) == origin {
			return true
		}
	}

	return false
}
