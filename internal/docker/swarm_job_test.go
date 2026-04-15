package docker

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

func TestRunSwarmJob(t *testing.T) {
	t.Parallel()

	dockerCli, err := CreateDockerCli(false, false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	if err := swarm.RefreshModeEnabled(t.Context(), dockerCli.Client()); err != nil {
		t.Errorf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	if !swarm.GetModeEnabled() {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	testCases := []struct {
		mode    swarm.DeployMode
		command []string
		title   string
	}{
		{mode: swarm.DeployModeGlobalJob, command: []string{"docker", "info"}, title: "global-docker-info"},
		{mode: swarm.DeployModeReplicatedJob, command: []string{"docker", "info"}, title: "replicated-docker-info"},
	}

	for _, tc := range testCases {
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Parallel()

			t.Logf("Running job with mode: %s, command: %v, title: %s", tc.mode, tc.command, tc.title)

			err := RunSwarmJob(t.Context(), dockerCli, tc.mode, tc.command, tc.title)
			if err != nil {
				t.Errorf("RunSwarmJob failed: %v", err)
			}
		})
	}
}

func TestRunImagePruneJob(t *testing.T) {
	t.Parallel()

	dockerCli, err := CreateDockerCli(false, false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	if err := swarm.RefreshModeEnabled(t.Context(), dockerCli.Client()); err != nil {
		t.Errorf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	if !swarm.GetModeEnabled() {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	err = RunImagePruneJob(t.Context(), dockerCli)
	if err != nil {
		t.Errorf("RunImagePruneJob failed: %v", err)
	}
}
