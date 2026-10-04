//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// TestAutoDiscoveryCleanup removes auto-discovered stacks from the repository
// and checks which of them doco-cd removes from the docker host. The removal
// policy comes from the current auto-discovery config that scans the stack's
// directory, not from the settings the stack was deployed with, and stopped
// stacks are removed too.
func TestAutoDiscoveryCleanup(t *testing.T) {
	t.Parallel()

	const (
		keepStack   = "e2e-ad-cleanup-keep"   // apps/, stays in the repository
		goneStack   = "e2e-ad-cleanup-gone"   // apps/, stopped and removed from the repository
		staticStack = "e2e-ad-cleanup-static" // static/, stays in the repository
		keptStack   = "e2e-ad-cleanup-kept"   // static/, removed from the repository
		service     = "app"
		staticApps  = "working_dir: static\nauto_discovery:\n  enabled: true"
		removedMsg  = "removed obsolete auto-discovered stack"
		keptMsg     = "skipping removal of obsolete auto-discovered stack as per configuration"
	)

	h := NewHarness(t, "auto-discovery-cleanup")
	for _, stack := range []string{keepStack, goneStack, staticStack, keptStack} {
		h.TrackStack(stack)
	}

	h.Start()

	containerID := func(stack string) func() string {
		return func() string { return h.ComposeContainerID(stack, service) }
	}

	h.WaitFor(2*time.Minute, "all discovered stacks deployed", func() bool {
		for _, stack := range []string{keepStack, goneStack, staticStack, keptStack} {
			if containerID(stack)() == "" {
				return false
			}
		}

		return true
	})

	keepID := containerID(keepStack)()
	staticID := containerID(staticStack)()
	keptID := containerID(keptStack)()

	if _, err := h.docker.ContainerStop(h.ctx, containerID(goneStack)(), client.ContainerStopOptions{}); err != nil {
		t.Fatalf("stop %s: %v", goneStack, err)
	}

	mark := h.LogMark()
	h.RemoveFromWorktree("apps/" + goneStack)
	h.RemoveFromWorktree("static/" + keptStack)
	h.RepoPush("remove two discovered stacks")

	h.WaitForLogLineAfter(mark, 2*time.Minute, removedMsg, `"stack":"`+goneStack+`"`)
	h.WaitForComposeContainerRemoval(goneStack, service, time.Minute)
	h.WaitForLogLineAfter(mark, 2*time.Minute, keptMsg, `"stack":"`+keptStack+`"`)

	h.AssertStays(5*time.Second, "stack the delete-disabled config scans is kept", keptID, containerID(keptStack))

	// The kept stack was deployed with delete disabled. Enabling it in the
	// config that scans the stack's directory now removes it.
	mark = h.LogMark()
	h.ReplaceInWorktree(".doco-cd.yml", staticApps, staticApps+"\n  delete: true")
	h.RepoPush("enable delete for static apps")

	h.WaitForLogLineAfter(mark, 2*time.Minute, removedMsg, `"stack":"`+keptStack+`"`)
	h.WaitForComposeContainerRemoval(keptStack, service, time.Minute)

	// The auto-discovery settings are stored in a label, so the remaining
	// stack of the changed config is recreated rather than removed.
	h.WaitForComposeContainerRecreate(staticStack, service, staticID, 2*time.Minute)

	if got := containerID(keepStack)(); got != keepID {
		t.Fatalf("%s container = %q, want %q", keepStack, got, keepID)
	}
}
