// Package logger provides a context-aware [slog.Logger] wrapper.
package logger

import (
	"context"
	"log/slog"
)

type contextKey struct{}

//nolint:gochecknoglobals // loggerKey is used as a context key for logger propagation
var loggerKey = contextKey{}

// Save attaches a logger to the context.
func Save(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// Load retrieves the logger from the context, or returns [slog.Default] if none is found.
func Load(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
