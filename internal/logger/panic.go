package logger

import (
	"log/slog"
	"runtime/debug"
)

// LogRecoveredPanic logs a recovered panic value with a stack trace.
func LogRecoveredPanic(log *slog.Logger, context string, recovered any) {
	log.Error("recovered from panic in background task",
		slog.String("context", context),
		slog.Any("panic", recovered),
		slog.String("stack", string(debug.Stack())),
	)
}
