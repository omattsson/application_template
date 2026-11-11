package handlers

import (
	"net/http"

	"backend/internal/config"

	"github.com/gin-gonic/gin"
)

// uiConfigCacheControl lets browsers and proxies keep the UI config for five
// minutes. A change shows after at most that time.
const uiConfigCacheControl = "public, max-age=300"

// UIConfigResponse holds the display values of the web UI. It contains no
// secret and no user data: the endpoint is public.
type UIConfigResponse struct {
	// Title is the product name (APP_TITLE).
	Title string `json:"title" example:"Application Template"`
	// AuthEnabled tells the UI whether to show the login flow (AUTH_ENABLED).
	AuthEnabled bool `json:"auth_enabled" example:"false"`
	// OIDCEnabled tells the UI to offer the single sign-on button
	// (AUTH_ENABLED and OIDC_ENABLED).
	OIDCEnabled bool `json:"oidc_enabled" example:"false"`
	// LocalAuthEnabled tells the UI to show the username and password form:
	// AUTH_ENABLED, and OIDC off or OIDC_LOCAL_AUTH=true. With OIDC on and
	// OIDC_LOCAL_AUTH=false only a service account (the bootstrap admin) can
	// still log in with a password, through the API.
	LocalAuthEnabled bool `json:"local_auth_enabled" example:"false"`
}

// UIConfigFromConfig returns the UI config of cfg. With AUTH_ENABLED=false
// every auth flag is false (the OIDC settings are not used then). An empty
// title gets the default (config.DefaultAppTitle).
func UIConfigFromConfig(cfg *config.Config) UIConfigResponse {
	if cfg == nil {
		return UIConfigResponse{Title: config.DefaultAppTitle}
	}
	resp := UIConfigResponse{Title: cfg.App.Title, AuthEnabled: cfg.Auth.Enabled}
	if resp.Title == "" {
		resp.Title = config.DefaultAppTitle
	}
	if cfg.Auth.Enabled {
		resp.OIDCEnabled = cfg.OIDC.Enabled
		resp.LocalAuthEnabled = !cfg.OIDC.Enabled || cfg.OIDC.LocalAuth
	}
	return resp
}

// NewUIConfigHandler returns the handler of GET /api/v1/ui-config that
// answers resp. An empty title gets the default (config.DefaultAppTitle).
func NewUIConfigHandler(resp UIConfigResponse) gin.HandlerFunc {
	if resp.Title == "" {
		resp.Title = config.DefaultAppTitle
	}

	return func(c *gin.Context) {
		getUIConfig(c, resp)
	}
}

// getUIConfig godoc
// @Summary      Web UI display config
// @Description  Returns the display values of the web UI: the product title (APP_TITLE), whether authentication is enabled (AUTH_ENABLED), whether the single sign-on login is offered (oidc_enabled: AUTH_ENABLED and OIDC_ENABLED) and whether the password form is shown (local_auth_enabled: AUTH_ENABLED, and OIDC off or OIDC_LOCAL_AUTH). Public in both auth modes: no authentication, so the login page can use it. Cached for 5 minutes.
// @Tags         ui
// @Produce      json
// @Success      200  {object}  UIConfigResponse
// @Failure      429  {object}  map[string]string
// @Router       /api/v1/ui-config [get]
func getUIConfig(c *gin.Context, resp UIConfigResponse) {
	c.Header("Cache-Control", uiConfigCacheControl)
	c.JSON(http.StatusOK, resp)
}
