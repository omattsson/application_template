package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"backend/internal/ctxutil"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type detachCtxKey struct{}

func TestDetachedCtxSurvivesRequestCancel(t *testing.T) {
	t.Parallel()

	parent, cancelRequest := context.WithCancel(context.WithValue(context.Background(), detachCtxKey{}, "req-1"))
	defer cancelRequest()

	req := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(parent)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	start := time.Now()
	ctx, cancel := detachedCtx(c)
	defer cancel()

	// The client leaves: the detached context stays alive.
	cancelRequest()
	require.Error(t, parent.Err())
	assert.NoError(t, ctx.Err(), "the request cancel must not end the detached context")

	// Values of the request (request ID, trace) are kept.
	assert.Equal(t, "req-1", ctx.Value(detachCtxKey{}))

	// It is bounded by the write timeout.
	deadline, ok := ctx.Deadline()
	require.True(t, ok, "the detached context has a deadline")
	assert.WithinDuration(t, start.Add(ctxutil.WriteTimeout), deadline, 5*time.Second)

	// The caller's cancel ends it.
	cancel()
	assert.ErrorIs(t, ctx.Err(), context.Canceled)
}
