//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// TestSwarmUnrelatedChange updates one service of a Swarm stack and checks
// that the tasks of the other services, including one with a named volume, are
// not replaced (#1909).
func TestSwarmUnrelatedChange(t *testing.T) {
	t.Parallel()

	const stack = "e2e-swarm-unrelated-change"

	h := NewHarness(t, "swarm-unrelated-change")
	if !h.isSwarmMode() {
		t.Skip("requires a Swarm manager")
	}

	h.TrackVolume(stack + "_data")
	h.Start()
	h.WaitForLog(deployCompletedLog, 2*time.Minute)

	task := func(service string) func() string {
		return func() string {
			return fmt.Sprintf("%s (%d tasks)", h.SwarmContainerID(stack, service), h.SwarmServiceTaskCount(stack, service))
		}
	}

	appTask := task("app")()
	changedID := h.SwarmContainerID(stack, "changed")

	mark := h.LogMark()
	h.ReplaceInWorktree("deploy/compose.yaml", `VERSION: "1"`, `VERSION: "2"`)
	h.RepoPush("update changed service")

	h.WaitForLogAfter(deployCompletedLog, mark, 2*time.Minute)
	h.WaitForContainerRecreate(stack, "changed", changedID, 2*time.Minute)
	h.AssertStays(15*time.Second, "unchanged service keeps its task", appTask, task("app"))
}
