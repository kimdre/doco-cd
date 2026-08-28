//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	internalDocker "github.com/kimdre/doco-cd/internal/docker"
)

func TestDeploymentModeMigration(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		scenario    string
		stackName   string
		volumeName  string
		sourceSwarm bool
	}{
		{
			name:       "compose_to_swarm",
			scenario:   "deployment-mode-migration",
			stackName:  "e2e-deployment-mode-migration",
			volumeName: "e2e-deployment-mode-migration-data",
		},
		{
			name:        "swarm_to_compose",
			scenario:    "deployment-mode-migration-swarm",
			stackName:   "e2e-deployment-mode-migration-swarm",
			volumeName:  "e2e-deployment-mode-migration-swarm-data",
			sourceSwarm: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testDeploymentModeMigration(t, tc.scenario, tc.stackName, tc.volumeName, tc.sourceSwarm)
		})
	}
}

func testDeploymentModeMigration(t *testing.T, scenario, stackName, volumeName string, sourceSwarm bool) {
	t.Helper()

	const (
		serviceName = "app"
		marker      = "migration-data"
	)

	h := NewHarness(t, scenario)
	if !h.isSwarmMode() {
		t.Skip("deployment mode migration requires a Swarm manager")
	}

	h.TrackVolume(volumeName)
	h.Start()

	sourceContainerID := func() string {
		if sourceSwarm {
			return h.SwarmContainerID(stackName, serviceName)
		}

		return h.ComposeContainerID(stackName, serviceName)
	}
	targetContainerID := func() string {
		if sourceSwarm {
			return h.ComposeContainerID(stackName, serviceName)
		}

		return h.SwarmContainerID(stackName, serviceName)
	}

	h.WaitFor(2*time.Minute, "initial deployment", func() bool {
		return sourceContainerID() != ""
	})

	if _, err := internalDocker.ExecContext(h.ctx, h.docker, sourceContainerID(),
		"sh", "-c", "printf "+marker+" >/data/marker"); err != nil {
		t.Fatalf("write source volume marker: %v", err)
	}

	logOffset := len(h.daemonLogs())
	if sourceSwarm {
		h.ReplaceInWorktree(".doco-cd.yml", "enabled: true", "enabled: false")
	} else {
		h.ReplaceInWorktree(".doco-cd.yml", "enabled: false", "enabled: true")
	}

	h.RepoPush("change deployment mode")

	h.WaitFor(2*time.Minute, "deployment mode migration", func() bool {
		return targetContainerID() != "" && sourceContainerID() == ""
	})
	waitForSuccessfulMigration(h, logOffset)
	assertMigrationMarker(t, h, targetContainerID(), marker)
}

func waitForSuccessfulMigration(h *Harness, logOffset int) {
	h.t.Helper()

	h.WaitFor(2*time.Minute, "migration deployment completed", func() bool {
		logs := h.daemonLogs()
		if len(logs) < logOffset {
			return false
		}

		logs = logs[logOffset:]

		migrationIndex := strings.Index(logs, `"msg":"removing previous deployment mode before migration"`)
		if migrationIndex < 0 {
			return false
		}

		return strings.Contains(logs[migrationIndex:], `"msg":"job completed successfully"`)
	})
}

func assertMigrationMarker(t *testing.T, h *Harness, containerID, want string) {
	t.Helper()

	output, err := internalDocker.ExecContext(h.ctx, h.docker, containerID, "cat", "/data/marker")
	if err != nil {
		t.Fatalf("read migration volume marker: %v", err)
	}

	if got := strings.TrimSpace(output); got != want {
		t.Fatalf("migration volume marker = %q, want %q", got, want)
	}
}
