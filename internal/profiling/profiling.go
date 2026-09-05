package profiling

import (
	"fmt"
	"log/slog"
	"net/http"
	httpPprof "net/http/pprof"
	"time"

	"github.com/kimdre/doco-cd/internal/graceful"
	"github.com/kimdre/doco-cd/internal/logger"
)

const (
	LoopbackAddress = "127.0.0.1"
	Path            = "/debug/pprof/"
)

// RegisterServer registers a loopback-only server for Go runtime profiles.
func RegisterServer(port uint16, log *logger.Logger) {
	log.Info("serving Go runtime profiles",
		slog.String("address", LoopbackAddress),
		slog.Int("http_port", int(port)),
		slog.String("path", Path),
	)

	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", LoopbackAddress, port),
		ReadHeaderTimeout: 3 * time.Second,
		Handler:           newHandler(),
	}

	graceful.RegisterServer(graceful.NewHttpServer("pprof", server, "", ""))
}

func newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(Path, httpPprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", httpPprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", httpPprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", httpPprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", httpPprof.Trace)

	return mux
}
