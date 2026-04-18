package logger

import (
	"context"
	"fmt"
	"http-protocol-deep-dive/internal/tracing"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"time"
)

type Logger struct {
	h loggerHandler
}

func (l *Logger) Debug(ctx context.Context, msg string, attr ...any) {
	l.write(ctx, LevelDebug, 3, msg, attr...)
}

func (l *Logger) Info(ctx context.Context, msg string, attr ...any) {
	l.write(ctx, LevelInfo, 3, msg, attr...)
}
func (l *Logger) Warn(ctx context.Context, msg string, attr ...any) {
	l.write(ctx, LevelWarn, 3, msg, attr...)
}
func (l *Logger) Error(ctx context.Context, msg string, attr ...any) {
	l.write(ctx, LevelError, 3, msg, attr...)
}

type level int

const (
	LevelDebug level = level(slog.LevelDebug)
	LevelInfo  level = level(slog.LevelInfo)
	LevelWarn  level = level(slog.LevelWarn)
	LevelError level = level(slog.LevelError)
)

func (l *Logger) write(ctx context.Context, level level, skipPc int, msg string, attr ...any) {
	pcs := make([]uintptr, 1)
	runtime.Callers(skipPc, pcs[:])

	r := slog.NewRecord(time.Now(), slog.Level(level), msg, pcs[0])

	r.Add(attr...)

	trace, err := tracing.GetTrace(ctx)
	if err != nil {
		l.h.Handler.Handle(ctx, r)
		return
	}

	r.Add("trace", trace)

	l.h.Handler.Handle(ctx, r)
}

func New(w io.Writer, root string) *Logger {
	replaceAttr := func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.SourceKey {

			if source, ok := a.Value.Any().(*slog.Source); ok {
				path, _ := filepath.Rel(root, source.File)

				return slog.Attr{
					Key:   "file",
					Value: slog.StringValue(fmt.Sprintf("%s:%d", path, source.Line)),
				}
			}
		}
		return a
	}

	options := &slog.HandlerOptions{
		AddSource:   true,
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
	}

	baseHandler := slog.NewJSONHandler(w, options)

	return &Logger{h: loggerHandler{Handler: baseHandler}}
}
