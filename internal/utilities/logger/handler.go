package logger

import (
	"context"
	"log/slog"
)

type loggerHandler struct {
	Handler slog.Handler
}

// WithAttrs implements [slog.Handler].
func (l *loggerHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return l.Handler.WithAttrs(attrs)
}

// WithGroup implements [slog.Handler].
func (l *loggerHandler) WithGroup(name string) slog.Handler {
	return l.Handler.WithGroup(name)
}

func (l *loggerHandler) Handle(ctx context.Context, r slog.Record) error {
	return l.Handler.Handle(ctx, r)
}

func (l *loggerHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return l.Handler.Enabled(ctx, level)
}
