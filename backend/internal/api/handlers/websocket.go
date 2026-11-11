package handlers

import (
	"context"
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
	// identityResolver reads the caller's identity (user, token, API key) from
	// the request context. Nil: the socket has an empty identity (auth off).
	identityResolver func(*gin.Context) websocket.ClientIdentity
	// recheck runs on the identity once the client is registered. Nil: no
	// re-check (auth off).
	recheck websocket.IdentityChecker
}

// SetRecheck sets the check that runs once a new socket is registered: a
// logout or a user revocation that ran between the authentication of the
// upgrade and the registration missed the socket, so the socket is checked
// once more and closed (1008) when the check reports it revoked. Pass the
// revalidation checker of the hub (NewWebSocketRevocationChecker), so the
// re-check applies the same policy. Call it before the server starts
// handling requests. Nil turns it off.
func (h *WebSocketHandler) SetRecheck(check websocket.IdentityChecker) {
	h.recheck = check
}

// SetIdentityResolver sets the function that derives the ClientIdentity of a
// new socket from the request. The hub uses the identity to address the socket
// by user and to close it on a revocation or when the credential expires. Call
// it before the server starts handling requests. A nil resolver means an empty
// identity.
func (h *WebSocketHandler) SetIdentityResolver(resolve func(*gin.Context) websocket.ClientIdentity) {
	h.identityResolver = resolve
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
// @Description Upgrades the HTTP connection to a WebSocket for real-time events. With AUTH_ENABLED=false no credential is needed and any topic can be subscribed. With AUTH_ENABLED=true the upgrade needs one credential: `Authorization: Bearer <jwt>`, `X-API-Key: sk_<key>` (header only), or `?token=<jwt>` for browsers (removed from the URL before any logging). The upgrade runs the checks of the HTTP auth (token, session and user blocklists, a deleted or disabled user, an unknown, deleted or expired key) and counts against the per-IP RATE_LIMIT. A failed blocklist or user lookup refuses the upgrade (500). An open socket is closed with code 1008 on logout, logout-all, a disable, delete, role change or password reset of the user, a deleted key, and when the token or key expires; a periodic re-check closes the sockets that another replica revoked. Subscriptions are authorised: items and item:<id> for every user, user:<id> for that user (and admins), admin:<name> for admins; any other topic is ignored.
// @Tags websocket
// @Param token query string false "Access token (AUTH_ENABLED=true, browsers)"
// @Param X-API-Key header string false "API key (AUTH_ENABLED=true, header only)"
// @Success 101 "Switching Protocols"
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 429 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Security BearerAuth
// @Security ApiKeyAuth
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

	var identity websocket.ClientIdentity
	if h.identityResolver != nil {
		identity = h.identityResolver(c)
	}
	client, err := websocket.NewClientWithIdentity(h.hub, conn, identity)
	if err != nil {
		slog.Error("WebSocket client creation failed", "error", err)
		return
	}
	h.recheckRegistered(c.Request.Context(), client, identity)
}

// recheckRegistered closes a new client whose identity the re-check reports
// revoked (see SetRecheck).
func (h *WebSocketHandler) recheckRegistered(ctx context.Context, client *websocket.Client, identity websocket.ClientIdentity) {
	if h.recheck == nil {
		return
	}
	if revoked := h.recheck(ctx, []websocket.ClientIdentity{identity}); len(revoked) == 1 && revoked[0] {
		slog.Info("WebSocket closed after registration: credential revoked", "user_id", identity.UserID)
		h.hub.DisconnectClient(client)
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
