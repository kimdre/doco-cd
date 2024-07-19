package main

import (
	"log/slog"
	"os"
)

func GetLogger() *slog.Logger {
	jsonHandler := slog.NewJSONHandler(os.Stderr, nil)
	log := slog.New(jsonHandler)

	return log
}
