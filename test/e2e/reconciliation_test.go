//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// TestReconciliation verifies that doco-cd observes a managed Compose
// container's die event and restores the missing service without recreating siblings.
func TestReconciliation(t *testing.T) {
	requireStandaloneSuite(t)
	t.Parallel()

	h := NewHarness(t, "reconciliation")
	h.Start()

	const (
		stack   = "e2e-reconciliation"
		service = "app"
	)

	h.WaitFor(120*time.Second, "initial service containers", func() bool {
		return h.ComposeContainerID(stack, service) != "" && h.ComposeContainerID(stack, "other") != ""
	})
	h.WaitForLog("reconciliation event listeners ready", 120*time.Second)
	oldID := h.ComposeContainerID(stack, service)
	siblingID := h.ComposeContainerID(stack, "other")
	mark := h.LogMark()

	if _, err := h.docker.ContainerRemove(h.ctx, oldID, client.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatalf("remove app container: %v", err)
	}

	h.WaitForLogAfter("reconciliation started", mark, 60*time.Second)
	h.WaitFor(90*time.Second, stack+"/"+service+" recreated", func() bool {
		id := h.ComposeContainerID(stack, service)
		return id != "" && id != oldID
	})
	h.WaitForLogAfter("reconciliation completed", mark, 90*time.Second)
	h.AssertStays(5*time.Second, "unchanged sibling container", siblingID, func() string {
		return h.ComposeContainerID(stack, "other")
	})
}
