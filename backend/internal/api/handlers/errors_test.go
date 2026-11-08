package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"backend/pkg/dberrors"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMapError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		wantCode int
		wantMsg  string
	}{
		{name: "nil", err: nil, wantCode: http.StatusOK, wantMsg: ""},
		{
			name:     "validation",
			err:      dberrors.NewDatabaseError("create", dberrors.ErrValidation),
			wantCode: http.StatusBadRequest,
			wantMsg:  "create: validation error",
		},
		{
			name:     "not found",
			err:      dberrors.NewDatabaseError("find", dberrors.ErrNotFound),
			wantCode: http.StatusNotFound,
			wantMsg:  "Widget not found",
		},
		{
			name:     "duplicate",
			err:      dberrors.NewDatabaseError("create", dberrors.ErrDuplicateKey),
			wantCode: http.StatusConflict,
			wantMsg:  "Widget already exists",
		},
		{
			name:     "not implemented",
			err:      fmt.Errorf("wrapped: %w", dberrors.ErrNotImplemented),
			wantCode: http.StatusNotImplemented,
			wantMsg:  "Feature not implemented",
		},
		{
			name:     "plain not found string",
			err:      errors.New("widget not found in db"),
			wantCode: http.StatusNotFound,
			wantMsg:  "Widget not found",
		},
		{
			name:     "plain duplicate string is not classified",
			err:      errors.New("duplicate entry"),
			wantCode: http.StatusInternalServerError,
			wantMsg:  "Internal server error",
		},
		{
			name:     "plain already exists string is not classified",
			err:      errors.New("widget already exists"),
			wantCode: http.StatusInternalServerError,
			wantMsg:  "Internal server error",
		},
		{
			name:     "plain validation string is not classified and never echoed",
			err:      errors.New("validation of secret-column failed"),
			wantCode: http.StatusInternalServerError,
			wantMsg:  "Internal server error",
		},
		{
			name:     "database error with unknown cause mentioning not found",
			err:      dberrors.NewDatabaseError("find", errors.New("peer not found")),
			wantCode: http.StatusInternalServerError,
			wantMsg:  "Internal server error",
		},
		{
			name:     "wrapped database error with unknown cause mentioning not found",
			err:      fmt.Errorf("outer: %w", dberrors.NewDatabaseError("find", errors.New("peer not found"))),
			wantCode: http.StatusInternalServerError,
			wantMsg:  "Internal server error",
		},
		{
			name:     "database error with sentinel-less operation text mentioning not found",
			err:      dberrors.NewDatabaseError("not found", errors.New("other")),
			wantCode: http.StatusInternalServerError,
			wantMsg:  "Internal server error",
		},
		{
			name:     "plain not found text keeps answering 404",
			err:      errors.New("item not found in db"),
			wantCode: http.StatusNotFound,
			wantMsg:  "Widget not found",
		},
		{
			name:     "database error wrapping validation keeps its text",
			err:      fmt.Errorf("handler: %w", dberrors.NewDatabaseError("create", dberrors.ErrValidation)),
			wantCode: http.StatusBadRequest,
			wantMsg:  "create: validation error",
		},
		{
			name:     "plain wrapped validation sentinel does not leak the wrapper context",
			err:      fmt.Errorf("column secret_token rejected: %w", dberrors.ErrValidation),
			wantCode: http.StatusBadRequest,
			wantMsg:  "validation failed",
		},
		{
			name:     "other error never leaks its message",
			err:      errors.New("dial tcp 10.0.0.1:3306: connection refused"),
			wantCode: http.StatusInternalServerError,
			wantMsg:  "Internal server error",
		},
		{
			name:     "other database error never leaks its message",
			err:      dberrors.NewDatabaseError("find", errors.New("secret detail")),
			wantCode: http.StatusInternalServerError,
			wantMsg:  "Internal server error",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, msg := mapError(tt.err, "Widget")
			assert.Equal(t, tt.wantCode, code)
			assert.Equal(t, tt.wantMsg, msg)
		})
	}
}

func TestMapError_RequestContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		wantCode int
		wantMsg  string
	}{
		{name: "cancelled", err: context.Canceled, wantCode: statusClientClosedRequest, wantMsg: msgClientClosedRequest},
		{
			name:     "cancelled in a database error",
			err:      dberrors.NewDatabaseError("find", context.Canceled),
			wantCode: statusClientClosedRequest, wantMsg: msgClientClosedRequest,
		},
		{
			name:     "cancelled before the string fallbacks",
			err:      fmt.Errorf("record not found: %w", context.Canceled),
			wantCode: statusClientClosedRequest, wantMsg: msgClientClosedRequest,
		},
		{name: "deadline exceeded", err: context.DeadlineExceeded, wantCode: http.StatusInternalServerError, wantMsg: msgInternalServerError},
		{
			name:     "deadline exceeded in a database error",
			err:      dberrors.NewDatabaseError("update", context.DeadlineExceeded),
			wantCode: http.StatusInternalServerError, wantMsg: msgInternalServerError,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, msg := mapError(tt.err, "Widget")
			assert.Equal(t, tt.wantCode, code)
			assert.Equal(t, tt.wantMsg, msg)
		})
	}
}

func TestCancelledRequestError(t *testing.T) {
	t.Parallel()

	status, msg, ok := cancelledRequestError(dberrors.NewDatabaseError("find", context.Canceled))
	assert.True(t, ok)
	assert.Equal(t, 499, status)
	assert.Equal(t, "Client closed request", msg)

	for _, err := range []error{nil, context.DeadlineExceeded, errors.New("boom")} {
		status, msg, ok := cancelledRequestError(err)
		assert.False(t, ok, "%v", err)
		assert.Zero(t, status)
		assert.Empty(t, msg)
	}
}

func TestRespondCancelled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		want     bool
		wantCode int
	}{
		{name: "cancelled", err: context.Canceled, want: true, wantCode: 499},
		{name: "deadline", err: context.DeadlineExceeded, want: false, wantCode: http.StatusOK},
		{name: "other", err: errors.New("boom"), want: false, wantCode: http.StatusOK},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

			got := respondCancelled(c, tt.err)
			assert.Equal(t, tt.want, got)
			if !tt.want {
				assert.False(t, c.Writer.Written())
				return
			}
			assert.Equal(t, tt.wantCode, w.Code)
			var body map[string]string
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, "Client closed request", body["error"])
		})
	}
}

// TestCancelledRequestError_LogLevel checks that a cancelled request logs one
// debug line and a passed deadline logs nothing in mapError and handleDBError.
// Not parallel: it replaces the default slog logger.
func TestCancelledRequestError_LogLevel(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mappers := map[string]func(error) (int, string){
		"mapError":      func(err error) (int, string) { return mapError(err, "Widget") },
		"handleDBError": handleDBError,
	}
	tests := []struct {
		name    string
		err     error
		wantLog bool
	}{
		{name: "cancelled", err: dberrors.NewDatabaseError("list", context.Canceled), wantLog: true},
		{name: "deadline exceeded", err: dberrors.NewDatabaseError("list", context.DeadlineExceeded)},
	}

	for _, tt := range tests {
		for mapperName, mapper := range mappers {
			t.Run(tt.name+"/"+mapperName, func(t *testing.T) {
				buf.Reset()
				mapper(tt.err)
				if tt.wantLog {
					assert.Contains(t, buf.String(), `level=DEBUG msg="Request cancelled by the client"`)
					assert.Equal(t, 1, strings.Count(buf.String(), "\n"), "one log line")
				} else {
					assert.Empty(t, buf.String())
				}
			})
		}
	}
}
