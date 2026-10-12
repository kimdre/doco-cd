package mcp

import (
	"context"
	"log/slog"
)

// sdkLogHandler keeps routine SDK messages at DEBUG without changing application logs.
type sdkLogHandler struct {
	next slog.Handler
}

func (h *sdkLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, sdkLogLevel(level))
}

func (h *sdkLogHandler) Handle(ctx context.Context, record slog.Record) error {
	record.Level = sdkLogLevel(record.Level)

	return h.next.Handle(ctx, record)
}

func (h *sdkLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &sdkLogHandler{next: h.next.WithAttrs(attrs)}
}

func (h *sdkLogHandler) WithGroup(name string) slog.Handler {
	return &sdkLogHandler{next: h.next.WithGroup(name)}
}

func sdkLogLevel(level slog.Level) slog.Level {
	if level == slog.LevelInfo {
		return slog.LevelDebug
	}

	return level
}
