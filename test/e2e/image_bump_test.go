//go:build e2e

package e2e

import (
	"testing"
	"time"
)

func TestImageBump(t *testing.T) {
	t.Parallel()

	const (
		stack     = "e2e-image-digest-bump"
		oldDigest = "sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d"
		newDigest = "sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce"
	)

	h := NewHarness(t, "image-digest-bump")
	h.Start()
	h.WaitFor(120*time.Second, "initial app container", func() bool {
		return h.ContainerID(stack, "app") != ""
	})
	oldID := h.ContainerID(stack, "app")
	logMark := h.LogMark()

	h.ReplaceInWorktree("deploy/compose.yaml", oldDigest, newDigest)
	h.RepoPush("bump app image digest")

	h.WaitForLogAfter(`"msg":"service image reference changed"`, logMark, 120*time.Second)
	h.WaitForContainerRecreate(stack, "app", oldID, 120*time.Second)
	h.WaitForLogAfter(`"msg":"job completed successfully"`, logMark, 120*time.Second)

	newID := h.ContainerID(stack, "app")
	if got, want := h.ContainerImage(newID), "alpine:latest@"+newDigest; !imageReferenceMatches(got, want) {
		t.Fatalf("updated container image = %q, want %q", got, want)
	}
}
