package swarm

import (
	"testing"

	"github.com/moby/moby/client"
)

func resolveTestSwarmMode(t *testing.T, apiClient client.APIClient) bool {
	t.Helper()

	enabled, err := ResolveModeEnabled(t.Context(), apiClient)
	if err != nil {
		t.Fatalf("failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	return enabled
}

func TestResolveModeEnabled(t *testing.T) {
	t.Parallel()

	dockerCli := getDockerClient(t)

	_ = resolveTestSwarmMode(t, dockerCli)
}
