package logger

import (
	"log/slog"
	"os"
)

func ParseLevel(s string) (slog.Level, error) {
	var level slog.Level
	err := level.UnmarshalText([]byte(s))

	return level, err
}

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
