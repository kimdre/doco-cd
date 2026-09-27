//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// TestComposeRelativePathsSurviveUnrelatedChange covers #1911: every commit deploys
// from own artifacts/<sha> dir, so services with repo relative bind mount or env_file
// got recreated when commit changed only another service.
func TestComposeRelativePathsSurviveUnrelatedChange(t *testing.T) {
	t.Parallel()

	const stack = "e2e-compose-relative-paths"

	untouched := []string{"plain", "bind-mount", "env-file"}

	h := NewHarness(t, "compose-relative-paths")
	h.Start()

	h.WaitForLog(`"msg":"job completed successfully"`, 2*time.Minute)

	before := make(map[string]string, len(untouched))
	for _, service := range untouched {
		before[service] = h.ComposeContainerID(stack, service)
		if before[service] == "" {
			t.Fatalf("%s container must run after the initial deployment", service)
		}
	}

	oldBumpedID := h.ComposeContainerID(stack, "bumped")

	h.ReplaceInWorktree("deploy/compose.yaml", "alpine:3.21", "alpine:3.22")
	h.RepoPush("bump unrelated service image")

	h.WaitFor(2*time.Minute, stack+"/bumped recreated", func() bool {
		id := h.ComposeContainerID(stack, "bumped")
		return id != "" && id != oldBumpedID
	})
	// Mark after the recreate, so an earlier no-op poll cannot end the wait.
	h.WaitForLogAfter(`"msg":"job completed successfully"`, h.LogMark(), 2*time.Minute)

	for _, service := range untouched {
		if got := h.ComposeContainerID(stack, service); got != before[service] {
			t.Errorf("%s recreated by unrelated change: container %s -> %s",
				service, shortContainerID(before[service]), shortContainerID(got))
		}
	}
}
