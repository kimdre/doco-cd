package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/graceful"
	"github.com/kimdre/doco-cd/internal/logger"
)

func registryApiServer(c *app.Config, h *handlerData, log *logger.Logger) {
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
		// ReadTimeout bounds the time to read the request (headers + body) to
		// mitigate slow-body (Slowloris) attacks. The webhook body is already
		// capped via MaxBytesReader, so a generous limit is sufficient.
		ReadTimeout: 30 * time.Second,
		// IdleTimeout bounds how long keep-alive connections may stay idle.
		IdleTimeout: 120 * time.Second,
		// WriteTimeout is intentionally left unset (0): synchronous deployments
		// triggered via ?wait=true can legitimately run for several minutes.
		Handler: apiServerMux,
	}

	graceful.RegisterServer(graceful.NewHttpServer("api", server))
}
