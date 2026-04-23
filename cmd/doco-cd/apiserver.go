package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/graceful"
	"github.com/kimdre/doco-cd/internal/logger"
)

func registryApiServer(c *config.AppConfig, h *handlerData, log *logger.Logger) {
	// Register API endpoints
	apiServerMux := http.NewServeMux()
	enabledApiEndpoints := registerApiEndpoints(c, h, log, apiServerMux)

	log.Info(
		"listening for events",
		slog.Int("http_port", int(c.HttpPort)),
		slog.Any("enabled_endpoints", enabledApiEndpoints),
	)

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", c.HttpPort),
		ReadHeaderTimeout: 3 * time.Second,
		Handler:           apiServerMux,
	}

	graceful.RegisterServer(graceful.NewHttpServer("api", server))
}
