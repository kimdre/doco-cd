package server

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// GetProviderFunc constructs a Provider for the plugin binary. Implementations
// load their own configuration from the environment and return a ready-to-serve
// provider.
type GetProviderFunc func(ctx context.Context, logger *slog.Logger) (Provider, error)

// Run is the standard entry point for plugin binaries. It wires up logging,
// signal handling, endpoint discovery, and calls Serve.
//
// Plugin mains should be a thin wrapper:
//
//	func main() {
//	    if err := server.Run(Version, getProvider); err != nil {
//	        os.Exit(1)
//	    }
//	}
func Run(version string, getProvider GetProviderFunc) error {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	endpoint := EndpointFromEnv()

	provider, err := getProvider(ctx, logger)
	if err != nil {
		logger.Error("failed to construct provider", "err", err)
		return err
	}

	logger.Info("starting secret provider plugin",
		"provider", provider.Name(),
		"version", version,
		"endpoint", endpoint,
	)

	if err := Serve(ctx, Options{Endpoint: endpoint, Version: version}, provider); err != nil {
		logger.Error("plugin exited with error", "err", err)
		return err
	}

	return nil
}
