package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"moonwalk/internal/order"
	"moonwalk/internal/store"
)

// fail maps service errors to status codes; unexpected ones are logged and hidden behind a 500.
func fail(c *gin.Context, err error) {
	var invalid order.InvalidError
	switch {
	case errors.As(err, &invalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, store.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	case errors.Is(err, order.ErrBadTransition), errors.Is(err, store.ErrExists):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, context.DeadlineExceeded):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "server busy, try again"})
	default:
		slog.Error("request failed", "method", c.Request.Method, "path", c.Request.URL.Path, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}

// recoverJSON turns a panic in a request into a 500 instead of a crash.
func recoverJSON(c *gin.Context) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("panic in request", "method", c.Request.Method, "path", c.Request.URL.Path, "panic", p)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
	}()
	c.Next()
}
