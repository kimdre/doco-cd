package logger

import (
	"log/slog"
	"os"
)

// ParseLevel parses a string into a log level
func ParseLevel(s string) (slog.Level, error) {
	var level slog.Level
	err := level.UnmarshalText([]byte(s))

	return level, err
}

// GetLogger returns a new logger with the given log level
func GetLogger(logLevel slog.Level) *slog.Logger {
	return slog.New(
		slog.NewJSONHandler(
			os.Stdout,
			&slog.HandlerOptions{
				// AddSource: true,
				Level: logLevel,
			},
		),
	)
}

// ErrAttr returns an attribute for an error
func ErrAttr(err error) slog.Attr {
	return slog.Any("error", err)
}
