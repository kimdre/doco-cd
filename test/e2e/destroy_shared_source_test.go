//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestDestroySharedSource destroys one of two stacks deployed from the same
// repository with the deprecated destroy.remove_dir option and checks that the
// bind-mounted repository files of the other stack survive (#1962). It then
// removes the published artifacts behind doco-cd's back, as older versions did,
// leaving the empty directories Docker re-creates for missing bind-mount
// sources, and checks that the next poll recreates the affected service from a
// freshly published artifact.
func TestDestroySharedSource(t *testing.T) {
	t.Parallel()

	const (
		keepStack = "e2e-destroy-shared-source-keep"
		goneStack = "e2e-destroy-shared-source-gone"
		service   = "app"
		message   = "keep-v1"
	)

	h := NewHarness(t, "destroy-shared-source")
	h.Start()

	containerID := func(stack string) func() string {
		return func() string { return h.ComposeContainerID(stack, service) }
	}

	h.WaitFor(2*time.Minute, "both stacks deployed", func() bool {
		return containerID(keepStack)() != "" && containerID(goneStack)() != ""
	})

	keepID := containerID(keepStack)()
	if got := h.ExecOutput(keepID, "cat", "/data/message.txt"); got != message {
		t.Fatalf("bind-mounted file = %q, want %q", got, message)
	}

	mark := h.LogMark()
	h.ReplaceInWorktree(
		".doco-cd.yml",
		"working_dir: gone",
		"working_dir: gone\ndestroy:\n  enabled: true\n  remove_dir: true",
	)
	h.RepoPush("destroy one stack")

	h.WaitForLogAfter("destroy.remove_dir is deprecated and ignored", mark, 2*time.Minute)
	h.WaitForContainerRemoval(goneStack, service, 2*time.Minute)

	h.AssertStays(5*time.Second, "stack sharing the source keeps its container", keepID, containerID(keepStack))

	if got := h.ExecOutput(keepID, "cat", "/data/message.txt"); got != message {
		t.Fatalf("bind-mounted file after destroying a sibling stack = %q, want %q", got, message)
	}

	// Docker re-creates a missing bind-mount source as an empty directory whenever the container restarts. The
	// re-created directories must not pass for the published artifact.
	mark = h.LogMark()
	removed := h.ModifyDataVolume(`set -e
dirs=$(find /data -type d -path '*/artifacts/*' -prune)
[ -n "$dirs" ]
rm -rf $dirs
for d in $dirs; do mkdir -p "$d/keep/data"; done
echo "$dirs"`)
	h.logf("removed artifacts: %s", strings.Join(strings.Fields(removed), ", "))

	h.WaitForLogAfter("artifact of deployed service is missing or was replaced since it was deployed", mark, 2*time.Minute)
	h.WaitForContainerRecreate(keepStack, service, keepID, 2*time.Minute)

	if got := h.ExecOutput(containerID(keepStack)(), "cat", "/data/message.txt"); got != message {
		t.Fatalf("bind-mounted file after recreating from a republished artifact = %q, want %q", got, message)
	}
}
