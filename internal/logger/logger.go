package logger

import (
	"log/slog"
	"os"
)

func GetLogger() *slog.Logger {
	log := slog.New(
		slog.NewJSONHandler(
			os.Stdout,
			&slog.HandlerOptions{
				// AddSource: true,
				Level: slog.LevelDebug,
			},
		),
	)

	return log
}
