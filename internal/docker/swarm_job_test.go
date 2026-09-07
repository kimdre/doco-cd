package docker

import (
	"strings"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

func TestSwarmOneOffServiceName(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 1730000000000000000)
	suffix := "-doco-job-1730000000000000000"

	testCases := []struct {
		name              string
		sourceServiceName string
		want              string
	}{
		{
			name:              "keeps short source service name",
			sourceServiceName: "stack_backup",
			want:              "stack_backup" + suffix,
		},
		{
			name:              "truncates source service name to Docker limit",
			sourceServiceName: strings.Repeat("a", maxSwarmServiceNameLength),
			want:              strings.Repeat("a", maxSwarmServiceNameLength-len(suffix)) + suffix,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := swarmOneOffServiceName(tc.sourceServiceName, now)

			if got != tc.want {
				t.Errorf("swarmOneOffServiceName() = %q, want %q", got, tc.want)
			}

			if len(got) > maxSwarmServiceNameLength {
				t.Errorf("swarmOneOffServiceName() length = %d, want <= %d", len(got), maxSwarmServiceNameLength)
			}
		})
	}
}

func TestRunSwarmJob(t *testing.T) {
	t.Parallel()

	dockerCli, err := CreateDockerCli(false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	if !resolveTestSwarmMode(t.Context(), t, dockerCli.Client()) {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	testCases := []struct {
		mode    swarm.DeployMode
		command []string
		title   string
		wantErr string
	}{
		{mode: swarm.DeployModeGlobalJob, command: []string{"docker", "info"}, title: "global-docker-info"},
		{mode: swarm.DeployModeReplicatedJob, command: []string{"docker", "info"}, title: "replicated-docker-info"},
		{mode: swarm.DeployModeReplicatedJob, command: []string{"sh", "-c", "exit 7"}, title: "replicated-exit-7", wantErr: "exit code 7"},
	}

	for _, tc := range testCases {
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Parallel()

			t.Logf("Running job with mode: %s, command: %v, title: %s", tc.mode, tc.command, tc.title)

			err := RunSwarmJob(t.Context(), dockerCli, tc.mode, tc.command, tc.title)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("RunSwarmJob failed: %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("RunSwarmJob error = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestRunImagePruneJob(t *testing.T) {
	t.Parallel()

	dockerCli, err := CreateDockerCli(false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	if !resolveTestSwarmMode(t.Context(), t, dockerCli.Client()) {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	err = RunImagePruneJob(t.Context(), dockerCli)
	if err != nil {
		t.Errorf("RunImagePruneJob failed: %v", err)
	}
}
