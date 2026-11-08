package handlers

import (
	"context"

	"backend/internal/ctxutil"

	"github.com/gin-gonic/gin"
)

// detachedCtx returns the request context without its cancel, limited to
// ctxutil.WriteTimeout: a client that leaves cannot skip a write or its side
// effects, and a stalled database cannot hold the request forever. A timeout
// ends the write with context.DeadlineExceeded, which mapError answers with
// 500. The caller must call the cancel function.
func detachedCtx(c *gin.Context) (context.Context, context.CancelFunc) {
	return ctxutil.Detach(c.Request.Context(), ctxutil.WriteTimeout)
}
