//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// TestComposeUnrelatedChange updates one service of a Compose stack and checks
// that a service with repository bind mounts and env files, both inside and
// outside the working directory, is only recreated once its own files change
// (#1911).
func TestComposeUnrelatedChange(t *testing.T) {
	t.Parallel()

	const stack = "e2e-compose-unrelated-change"

	h := NewHarness(t, "compose-unrelated-change")
	h.Start()
	h.WaitForLog(deployCompletedLog, 2*time.Minute)

	containerID := func(service string) func() string {
		return func() string { return h.ComposeContainerID(stack, service) }
	}
	waitForRecreate := func(service, oldID string) string {
		h.WaitFor(2*time.Minute, stack+"/"+service+" recreated", func() bool {
			id := h.ComposeContainerID(stack, service)
			return id != "" && id != oldID
		})

		return h.ComposeContainerID(stack, service)
	}

	staticID := containerID("static")()
	changedID := containerID("changed")()

	mark := h.LogMark()
	h.ReplaceInWorktree("deploy/compose.yaml", `VERSION: "1"`, `VERSION: "2"`)
	h.RepoPush("update changed service")

	h.WaitForLogAfter(deployCompletedLog, mark, 2*time.Minute)

	changedID = waitForRecreate("changed", changedID)

	h.AssertStays(5*time.Second, "service with unchanged repository files keeps its container", staticID, containerID("static"))

	mark = h.LogMark()
	h.ReplaceInWorktree("deploy/static/index.html", "index-v1", "index-v2")
	h.RepoPush("update static files")

	h.WaitForLogAfter(deployCompletedLog, mark, 2*time.Minute)

	staticID = waitForRecreate("static", staticID)

	h.AssertStays(5*time.Second, "unchanged service keeps its container", changedID, containerID("changed"))

	if got := h.ExecOutput(staticID, "cat", "/static/index.html"); got != "index-v2" {
		t.Fatalf("bind-mounted file = %q, want %q", got, "index-v2")
	}

	if got := h.ExecOutput(staticID, "cat", "/etc/app.conf"); got != "conf-v1" {
		t.Fatalf("bind-mounted single file = %q, want %q", got, "conf-v1")
	}

	if got := h.ExecOutput(staticID, "cat", "/shared/shared.conf"); got != "shared-v1" {
		t.Fatalf("bind-mounted file outside working dir = %q, want %q", got, "shared-v1")
	}

	if got := h.ExecOutput(staticID, "printenv", "GREETING"); got != "hello" {
		t.Fatalf("GREETING from env file = %q, want %q", got, "hello")
	}

	if got := h.ExecOutput(staticID, "printenv", "SHARED"); got != "shared-v1" {
		t.Fatalf("SHARED from env file outside working dir = %q, want %q", got, "shared-v1")
	}
}
