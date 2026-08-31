//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// TestReconciliation verifies that doco-cd observes a managed Compose
// container's die event and recreates the missing service.
func TestReconciliation(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "reconciliation")
	h.Start()

	const (
		stack   = "e2e-reconciliation"
		service = "app"
	)

	h.WaitFor(120*time.Second, "initial app container", func() bool {
		return h.ComposeContainerID(stack, service) != ""
	})
	h.WaitForLog("reconciliation event listeners ready", 120*time.Second)
	oldID := h.ComposeContainerID(stack, service)

	if _, err := h.docker.ContainerRemove(h.ctx, oldID, client.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatalf("remove app container: %v", err)
	}

	h.WaitForLog("reconciliation started", 60*time.Second)
	h.WaitFor(90*time.Second, stack+"/"+service+" recreated", func() bool {
		id := h.ComposeContainerID(stack, service)
		return id != "" && id != oldID
	})
}
