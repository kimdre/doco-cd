package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/kimdre/doco-cd/cmd/secretproviders/awssecretmanager/internal/awssecretsmanager"
	"github.com/kimdre/doco-cd/cmd/secretproviders/internal/server"
)

// Version is overridden at build time via -ldflags.
var Version = "dev"

func main() {
	if err := server.Run(Version, getProvider); err != nil {
		os.Exit(1)
	}
}

func getProvider(ctx context.Context, _ *slog.Logger) (server.Provider, error) {
	cfg, err := awssecretsmanager.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	provider, err := awssecretsmanager.NewProvider(ctx, cfg.Region, cfg.AccessKeyID, cfg.SecretAccessKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider: %w", err)
	}

	return provider, nil
}
