//go:build e2e

package e2e

import (
	"testing"
	"time"
)

func TestImageTagBump(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "image-tag-bump")
	h.Start()

	h.WaitFor(120*time.Second, "initial app container", func() bool {
		return h.ContainerID("e2e-image-tag-bump", "app") != ""
	})

	h.ReplaceInWorktree("deploy/compose.yaml", "alpine:3.21", "alpine:3.22")
	h.RepoPush("bump app image")

	h.WaitForLog(`"msg":"service image reference changed"`, 120*time.Second)
}
