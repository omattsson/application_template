package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"backend/pkg/dberrors"

	"github.com/gin-gonic/gin"
)

// Common error messages shared across handlers.
const (
	msgInternalServerError  = "Internal server error"
	msgInvalidRequestFormat = "Invalid request format"
	msgValidationFailed     = "validation failed"
	// msgClientClosedRequest is the message of statusClientClosedRequest.
	msgClientClosedRequest = "Client closed request"
	// statusClientClosedRequest is the nginx status for a request that the
	// client cancelled before the response (net/http has no constant for it).
	statusClientClosedRequest = 499
)

// mapError translates a repository error into an appropriate HTTP status code
// and a safe, user-facing message. entityName is used to build contextual
// "not found" / "already exists" messages (for example "Item").
func mapError(err error, entityName string) (int, string) {
	if err == nil {
		return http.StatusOK, ""
	}

	if status, msg, ok := cancelledRequestError(err); ok {
		return status, msg
	}

	// Classify by sentinel with errors.Is (works with wrapped errors).
	if errors.Is(err, dberrors.ErrValidation) {
		return http.StatusBadRequest, validationMessage(err)
	}
	if errors.Is(err, dberrors.ErrNotFound) {
		return http.StatusNotFound, entityName + " not found"
	}
	if errors.Is(err, dberrors.ErrDuplicateKey) {
		return http.StatusConflict, entityName + " already exists"
	}
	if errors.Is(err, dberrors.ErrNotImplemented) {
		return http.StatusNotImplemented, "Feature not implemented"
	}

	// Fallback for repositories that return a plain "not found" error without
	// a sentinel. A DatabaseError with an unknown cause is classified by its
	// cause only, never by its text.
	var dbErr *dberrors.DatabaseError
	if !errors.As(err, &dbErr) && strings.Contains(err.Error(), "not found") {
		return http.StatusNotFound, entityName + " not found"
	}

	// Never leak internal error details.
	return http.StatusInternalServerError, msgInternalServerError
}

// validationMessage returns the client message of a validation error. Only the
// text of a DatabaseError (operation and sentinel) is shown; context added by
// a plain wrapper is never exposed.
func validationMessage(err error) string {
	var dbErr *dberrors.DatabaseError
	if errors.As(err, &dbErr) {
		return dbErr.Error()
	}
	return msgValidationFailed
}

// cancelledRequestError maps an error caused by a cancelled request context
// to 499. A deadline that passed is not a client cancel and stays a 500.
// It assumes context.Canceled comes from the request; a server-side cancel
// also gives 499.
func cancelledRequestError(err error) (int, string, bool) {
	if !errors.Is(err, context.Canceled) {
		return 0, "", false
	}
	slog.Debug("Request cancelled by the client", "error", err)
	return statusClientClosedRequest, msgClientClosedRequest, true
}

// respondCancelled writes the 499 answer when err comes from a cancelled
// request context and reports whether it did. Use it where a handler would
// otherwise log the error and answer 500.
func respondCancelled(c *gin.Context, err error) bool {
	status, msg, ok := cancelledRequestError(err)
	if ok {
		c.JSON(status, gin.H{"error": msg})
	}
	return ok
}

// msgRequestBodyTooLarge is the message of a 413 answer; it matches the one of
// middleware.MaxBodySize.
const msgRequestBodyTooLarge = "request body too large"

// respondBindError writes the answer for a request body that could not be
// bound: 413 when the body is over the MaxBodySize limit, 400 otherwise.
func respondBindError(c *gin.Context, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": msgRequestBodyTooLarge})
		return
	}

	c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidRequestFormat})
}
