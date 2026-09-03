//go:build e2e

package e2e

import (
	"testing"
	"time"
)

func TestDeploymentLifecycle(t *testing.T) {
	t.Parallel()

	const (
		stack    = "e2e-deployment-lifecycle"
		service  = "app"
		oldImage = "alpine:3.22"
		newImage = "alpine:3.21"
	)

	h := NewHarness(t, "deployment-lifecycle")
	h.Start()

	h.WaitFor(2*time.Minute, "initial app container", func() bool {
		return h.ContainerID(stack, service) != ""
	})

	initialID := h.ContainerID(stack, service)
	if got := h.ContainerImage(initialID); !imageReferenceMatches(got, oldImage) {
		t.Fatalf("initial container image = %q, want %q", got, oldImage)
	}

	failureMark := h.LogMark()
	h.ReplaceInWorktree("deploy/compose.yaml", "image: "+oldImage, "image: [")
	h.RepoPush("break compose configuration")

	h.WaitForLogAfter(`"msg":"job completed with errors"`, failureMark, 2*time.Minute)

	if got := h.ContainerID(stack, service); got != initialID {
		t.Fatalf("invalid configuration changed last-known-good container: got %q, want %q", got, initialID)
	}

	recoveryMark := h.LogMark()
	h.ReplaceInWorktree("deploy/compose.yaml", "image: [", "image: "+newImage)
	h.RepoPush("repair compose configuration")

	h.WaitForContainerRecreate(stack, service, initialID, 2*time.Minute)
	h.WaitForLogAfter(`"msg":"job completed successfully"`, recoveryMark, 2*time.Minute)

	recoveredID := h.ContainerID(stack, service)
	if got := h.ContainerImage(recoveredID); !imageReferenceMatches(got, newImage) {
		t.Fatalf("recovered container image = %q, want %q", got, newImage)
	}

	destroyMark := h.LogMark()
	h.ReplaceInWorktree(
		".doco-cd.yml",
		"working_dir: deploy",
		"working_dir: deploy\ndestroy:\n  enabled: true\n  remove_images: false\n  remove_dir: false",
	)
	h.RepoPush("destroy deployment")

	h.WaitForLogAfter(`"msg":"destroying stack"`, destroyMark, 2*time.Minute)
	h.WaitForContainerRemoval(stack, service, 2*time.Minute)
}
