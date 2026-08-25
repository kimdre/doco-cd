//go:build e2e

package e2e

import (
	"testing"
	"time"
)

func TestImageDigestBump(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "image-digest-bump")
	h.Start()

	h.WaitFor(120*time.Second, "initial app container", func() bool {
		return h.ContainerID("e2e-image-digest-bump", "app") != ""
	})

	h.ReplaceInWorktree(
		"deploy/compose.yaml",
		"sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d",
		"sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce",
	)
	h.RepoPush("bump app image digest")

	h.WaitForLog(`"msg":"service image reference changed"`, 120*time.Second)
}
