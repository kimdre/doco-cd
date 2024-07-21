package logger

import (
	"log/slog"
	"os"
)

type Logger struct {
	*slog.Logger
}

// ParseLevel parses a string into a log level
func ParseLevel(s string) (slog.Level, error) {
	var level slog.Level
	err := level.UnmarshalText([]byte(s))

	return level, err
}

// ErrAttr returns an attribute for an error
func (l *Logger) ErrAttr(err error) slog.Attr {
	return slog.Any("error", err)
}

// New returns a new Logger with the given log level
func New(logLevel slog.Level) *Logger {
	return &Logger{
		slog.New(
			slog.NewJSONHandler(
				os.Stderr,
				&slog.HandlerOptions{
					// AddSource: true,
					Level: logLevel,
				},
			),
		),
	}
}
