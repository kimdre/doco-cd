package prometheus

import (
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	MetricsNamespace = "doco_cd"  // Namespace for all metrics
	MetricsPath      = "/metrics" // Path for exposing metrics via HTTP
)

// Serve starts the Prometheus metrics server.
func Serve(port uint16) error {
	mux := http.NewServeMux()
	mux.Handle(MetricsPath, promhttp.Handler())

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		ReadHeaderTimeout: 3 * time.Second,
		Handler:           mux,
	}

	http.Handle(MetricsPath, promhttp.Handler())

	err := server.ListenAndServe()
	if err != nil {
		return err
	}

	return nil
}
