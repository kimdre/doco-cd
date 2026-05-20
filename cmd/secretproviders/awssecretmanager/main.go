package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/kimdre/doco-cd/cmd/secretproviders/awssecretmanager/internal/awssecretsmanager"
	"github.com/kimdre/doco-cd/cmd/secretproviders/internal/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if err := run(context.Background(), logger); err != nil {
		logger.Error("plugin exited with error", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := awssecretsmanager.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	provider, err := awssecretsmanager.NewProvider(ctx, cfg.Region, cfg.AccessKeyID, cfg.SecretAccessKey)
	if err != nil {
		return fmt.Errorf("failed to create provider: %w", err)
	}

	endpoint := server.EndpointFromEnv()

	logger.Info("starting secret provider plugin", "provider", provider.Name(), "endpoint", endpoint)

	return server.Serve(ctx, server.Options{Endpoint: endpoint}, provider)
}
