//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// TestSwarmUnchangedServiceKeepsTasks covers #1909: cd.doco.deployment.working_dir
// held the per-commit artifacts/<sha> path in the task template, so swarm replaced
// tasks of every service on each deployment.
func TestSwarmUnchangedServiceKeepsTasks(t *testing.T) {
	t.Parallel()

	const stack = "e2e-swarm-unchanged-service"

	// "volume" covers the same label in the named volume's mount options.
	untouched := []string{"plain", "volume"}

	h := NewHarness(t, "swarm-unchanged-service")
	if !h.isSwarmMode() {
		t.Skip("requires a Swarm manager")
	}

	h.TrackVolume(stack + "-data")
	h.Start()

	h.WaitForLog(`"msg":"job completed successfully"`, 2*time.Minute)

	before := make(map[string]string, len(untouched))
	for _, service := range untouched {
		before[service] = h.SwarmContainerID(stack, service)
		if before[service] == "" {
			t.Fatalf("%s task must run after the initial deployment", service)
		}
	}

	oldBumpedID := h.SwarmContainerID(stack, "bumped")

	h.ReplaceInWorktree("deploy/compose.yaml", "alpine:3.21", "alpine:3.22")
	h.RepoPush("bump unrelated service image")

	h.WaitFor(2*time.Minute, stack+"/bumped task replaced", func() bool {
		id := h.SwarmContainerID(stack, "bumped")
		return id != "" && id != oldBumpedID
	})
	// Mark after the replace, so an earlier no-op poll cannot end the wait.
	// The deploy waits for convergence, so replaced tasks are running by then.
	h.WaitForLogAfter(`"msg":"job completed successfully"`, h.LogMark(), 2*time.Minute)

	for _, service := range untouched {
		if got := h.SwarmContainerID(stack, service); got != before[service] {
			t.Errorf("%s task replaced by unrelated change: container %s -> %s",
				service, shortContainerID(before[service]), shortContainerID(got))
		}
	}
}
