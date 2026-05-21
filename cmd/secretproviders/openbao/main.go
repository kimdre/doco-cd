package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/kimdre/doco-cd/cmd/secretproviders/internal/server"
	"github.com/kimdre/doco-cd/cmd/secretproviders/openbao/internal/openbao"
)

func main() {
	if err := server.Run(getProvider); err != nil {
		os.Exit(1)
	}
}

func getProvider(ctx context.Context, _ *slog.Logger) (server.Provider, error) {
	cfg, err := openbao.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	provider, err := openbao.NewProvider(ctx, cfg.SiteUrl, cfg.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider: %w", err)
	}

	return provider, nil
}
