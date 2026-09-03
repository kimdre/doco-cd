//go:build e2e

package e2e

import (
	"testing"
	"time"
)

func TestMultiContextSameNameDeployment(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "multi-context")
	h.EnableRemoteContext()
	h.Start()

	h.WaitFor(2*time.Minute, "same-named stack deployed on default context", func() bool {
		return h.ContainerID("e2e-multi-context", "app") != ""
	})
	h.WaitFor(2*time.Minute, "same-named stack deployed on remote context", func() bool {
		return h.RemoteContainerID("e2e-multi-context", "app") != ""
	})

	const (
		stack    = "e2e-multi-context"
		oldImage = "alpine:3.22"
		newImage = "alpine:3.21"
	)

	localID := h.ContainerID(stack, "app")
	remoteID := h.RemoteContainerID(stack, "app")
	logMark := h.LogMark()

	h.ReplaceInWorktree("deploy/compose.yaml", oldImage, newImage)
	h.RepoPush("update both contexts")

	h.WaitFor(2*time.Minute, "default-context deployment updated", func() bool {
		id := h.ContainerID(stack, "app")
		return id != "" && id != localID
	})
	h.WaitFor(2*time.Minute, "remote-context deployment updated", func() bool {
		id := h.RemoteContainerID(stack, "app")
		return id != "" && id != remoteID
	})
	h.WaitForLogAfter(`"msg":"job completed successfully"`, logMark, 2*time.Minute)

	if got := h.ContainerImage(h.ContainerID(stack, "app")); !imageReferenceMatches(got, newImage) {
		t.Fatalf("default-context container image = %q, want %q", got, newImage)
	}

	if got := h.RemoteContainerImage(h.RemoteContainerID(stack, "app")); !imageReferenceMatches(got, newImage) {
		t.Fatalf("remote-context container image = %q, want %q", got, newImage)
	}
}
