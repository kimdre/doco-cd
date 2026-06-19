package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/kimdre/doco-cd/cmd/secretproviders/bitwardensecretsmanager/internal/bitwardensecretsmanager"
	"github.com/kimdre/doco-cd/cmd/secretproviders/internal/server"
)

// Version is overridden at build time via -ldflags.
var Version = "dev"

func main() {
	if err := server.Run(Version, getProvider); err != nil {
		os.Exit(1)
	}
}

func getProvider(_ context.Context, _ *slog.Logger) (server.Provider, error) {
	cfg, err := bitwardensecretsmanager.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	provider, err := bitwardensecretsmanager.NewProvider(cfg.ApiUrl, cfg.IdentityUrl, cfg.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider: %w", err)
	}

	return provider, nil
}
