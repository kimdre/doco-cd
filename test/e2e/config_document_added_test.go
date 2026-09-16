//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// TestDeployConfigGainsDocument verifies that a second YAML document appended
// to a deploy config the daemon is already polling is deployed on the next
// poll, without restarting the daemon.
func TestDeployConfigGainsDocument(t *testing.T) {
	t.Parallel()

	const (
		firstStack  = "e2e-config-document-added-first"
		secondStack = "e2e-config-document-added-second"
		service     = "app"
	)

	h := NewHarness(t, "config-document-added")
	h.TrackStack(secondStack)
	h.Start()

	h.WaitFor(2*time.Minute, "initial stack container", func() bool {
		return h.ContainerID(firstStack, service) != ""
	})

	firstID := h.ContainerID(firstStack, service)
	logMark := h.LogMark()

	// The new stack's working directory arrives in the same commit as the new
	// document, the way a stack is added to a deployments repo in practice.
	h.WriteInWorktree("deploy/second/compose.yaml", `services:
  app:
    image: alpine:3.22
    command: ["sleep", "600"]
`)
	h.WriteInWorktree(".doco-cd.yml", `name: `+firstStack+`
working_dir: deploy/first
---
name: `+secondStack+`
working_dir: deploy/second
`)
	h.RepoPush("add a second deploy config document")

	h.WaitFor(2*time.Minute, "stack from the appended document deployed", func() bool {
		return h.ContainerID(secondStack, service) != ""
	})
	h.WaitForLogAfter(`"msg":"job completed successfully"`, logMark, 2*time.Minute)

	if got := h.ContainerID(firstStack, service); got != firstID {
		t.Fatalf("existing stack container changed: got %q, want %q", got, firstID)
	}
}
