package docker

import (
	"context"
	"testing"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

func resolveTestSwarmMode(ctx context.Context, t *testing.T, apiClient client.APIClient) bool {
	t.Helper()

	enabled, err := swarm.ResolveModeEnabled(ctx, apiClient)
	if err != nil {
		t.Fatalf("failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	return enabled
}
