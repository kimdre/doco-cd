package main

import (
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/api"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/logger"
)

func TestRegistryAPIServerPropagatesRouteRegistrationError(t *testing.T) {
	t.Parallel()

	err := registryApiServer(&app.Config{}, nil, api.Mounts{}, logger.New(logger.LevelCritical))
	if err == nil {
		t.Fatal("registryApiServer succeeded with a nil API handler")
	}

	if !strings.Contains(err.Error(), "register API routes") {
		t.Fatalf("registryApiServer error = %q", err)
	}
}
