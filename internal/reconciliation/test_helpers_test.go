package reconciliation

import (
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker"
	dockerSwarm "github.com/kimdre/doco-cd/internal/docker/swarm"
)

// resolveTestSwarmMode checks if the Docker daemon is in Swarm mode.
func resolveTestSwarmMode(t *testing.T, apiClient client.APIClient) bool {
	t.Helper()

	enabled, err := dockerSwarm.ResolveModeEnabled(t.Context(), apiClient)
	if err != nil {
		t.Fatalf("failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	return enabled
}

// newTestManagerWithDependencies creates a Manager for tests, filling in a zero-value AppConfig
// and a fresh Docker CLI (closed via t.Cleanup) for any dependency left unset in deps, so tests
// only need to override the fields they care about.
func newTestManagerWithDependencies(t *testing.T, deps Dependencies) *Manager {
	t.Helper()

	if deps.AppConfig == nil {
		deps.AppConfig = &app.Config{}
	}

	if deps.DataMountPoint.Source == "" || deps.DataMountPoint.Destination == "" {
		dataDir := t.TempDir()
		deps.DataMountPoint = container.MountPoint{
			Type:        "bind",
			Source:      dataDir,
			Destination: dataDir,
			Mode:        "rw",
		}
	}

	if deps.DockerCLI == nil {
		dockerCli, err := docker.CreateDockerCli(true)
		if err != nil {
			t.Fatalf("failed to create docker cli: %v", err)
		}

		t.Cleanup(func() { _ = dockerCli.Client().Close() })

		deps.DockerCLI = dockerCli
	}

	if deps.Contexts == nil {
		deps.Contexts = docker.NewContextRegistry(deps.DockerCLI, docker.ContextRegistryOptions{
			Quiet:         true,
			SwarmFeatures: true,
		})

		t.Cleanup(func() { _ = deps.Contexts.Close() })
	}

	manager, err := NewManager(deps)
	if err != nil {
		t.Fatalf("failed to create reconciliation manager: %v", err)
	}

	t.Cleanup(manager.Close)

	return manager
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()

	return newTestManagerWithDependencies(t, Dependencies{})
}

func newTestDeployerLimiter(t *testing.T, maxConcurrent uint) *DeployerLimiter {
	t.Helper()

	limiter := NewDeployerLimiter(maxConcurrent)
	t.Cleanup(limiter.Close)

	return limiter
}
