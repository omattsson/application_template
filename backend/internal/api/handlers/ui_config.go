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
}

// NewUIConfigHandler returns the handler of GET /api/v1/ui-config. An empty
// title gets the default (config.DefaultAppTitle).
func NewUIConfigHandler(title string, authEnabled bool) gin.HandlerFunc {
	resp := UIConfigResponse{Title: title, AuthEnabled: authEnabled}
	if resp.Title == "" {
		resp.Title = config.DefaultAppTitle
	}

	return func(c *gin.Context) {
		getUIConfig(c, resp)
	}
}

// getUIConfig godoc
// @Summary      Web UI display config
// @Description  Returns the display values of the web UI: the product title (APP_TITLE) and whether authentication is enabled (AUTH_ENABLED). Public: no authentication, so the login page can use it. Cached for 5 minutes.
// @Tags         ui
// @Produce      json
// @Success      200  {object}  UIConfigResponse
// @Failure      429  {object}  map[string]string
// @Router       /api/v1/ui-config [get]
func getUIConfig(c *gin.Context, resp UIConfigResponse) {
	c.Header("Cache-Control", uiConfigCacheControl)
	c.JSON(http.StatusOK, resp)
}
