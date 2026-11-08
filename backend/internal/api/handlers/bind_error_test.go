package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// limitBody caps the request body like middleware.MaxBodySize, without a
// declared-length check, so the handler itself reads past the limit.
func limitBody(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

func TestItemBindErrorStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantError  string
	}{
		{name: "create oversized", method: http.MethodPost, path: "/items",
			body: `{"name":"` + strings.Repeat("a", 200) + `"}`, wantStatus: http.StatusRequestEntityTooLarge, wantError: "request body too large"},
		{name: "update oversized", method: http.MethodPut, path: "/items/1",
			body: `{"name":"` + strings.Repeat("a", 200) + `"}`, wantStatus: http.StatusRequestEntityTooLarge, wantError: "request body too large"},
		{name: "create malformed", method: http.MethodPost, path: "/items",
			body: `{"name":`, wantStatus: http.StatusBadRequest, wantError: msgInvalidRequestFormat},
		{name: "update malformed", method: http.MethodPut, path: "/items/1",
			body: `not json`, wantStatus: http.StatusBadRequest, wantError: msgInvalidRequestFormat},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := NewMockRepository()
			h := NewHandler(repo)
			router := gin.New()
			router.Use(limitBody(64))
			router.POST("/items", h.CreateItem)
			router.PUT("/items/:id", h.UpdateItem)

			// Update binds after the lookup, so the item must exist.
			if tt.method == http.MethodPut {
				put := httptest.NewRecorder()
				seed := gin.New()
				seed.POST("/items", h.CreateItem)
				seed.ServeHTTP(put, httptest.NewRequest(http.MethodPost, "/items", strings.NewReader(`{"name":"seed","price":1}`)))
				assert.Equal(t, http.StatusCreated, put.Code)
			}

			w := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code)
			assert.JSONEq(t, `{"error":"`+tt.wantError+`"}`, w.Body.String())
		})
	}
}
