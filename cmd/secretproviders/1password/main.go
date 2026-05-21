package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	onepassword "github.com/kimdre/doco-cd/cmd/secretproviders/1password/internal/1password"
	"github.com/kimdre/doco-cd/cmd/secretproviders/internal/server"
)

// Version is overridden at build time via -ldflags.
var Version = "dev"

func main() {
	if err := server.Run(getProvider); err != nil {
		os.Exit(1)
	}
}

func getProvider(ctx context.Context, _ *slog.Logger) (server.Provider, error) {
	cfg, err := onepassword.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	provider, err := onepassword.NewProvider(ctx, cfg, Version)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider: %w", err)
	}

	return provider, nil
}
