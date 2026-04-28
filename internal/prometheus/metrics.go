package prometheus

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kimdre/doco-cd/internal/graceful"
	"github.com/kimdre/doco-cd/internal/logger"
)

const (
	MetricsNamespace = "doco_cd"  // Namespace for all metrics
	MetricsPath      = "/metrics" // Path for exposing metrics via HTTP
)

// RegisterServer registers the Prometheus metrics server.
func RegisterServer(port uint16, log *logger.Logger) {
	log.Info("serving prometheus metrics",
		slog.Int("http_port", int(port)),
		slog.String("path", MetricsPath),
	)

	mux := http.NewServeMux()
	mux.Handle(MetricsPath, promhttp.Handler())

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		ReadHeaderTimeout: 3 * time.Second,
		Handler:           mux,
	}

	graceful.RegisterServer(graceful.NewHttpServer("prometheus", server))
}
