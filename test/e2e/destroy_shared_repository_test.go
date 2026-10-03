//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker"
)

// TestDestroySharedRepository destroys one of two stacks that deploy from the same
// repository and checks that destroy.remove_dir keeps the repository directory while
// the other stack still mounts files from it (#1962). Once the last stack is destroyed,
// the directory is removed.
func TestDestroySharedRepository(t *testing.T) {
	t.Parallel()

	const (
		stackA         = "e2e-destroy-shared-repository-a"
		stackB         = "e2e-destroy-shared-repository-b"
		service        = "app"
		repositoryDir  = "/data/gitserver/destroy-shared-repository"
		keptLog        = `"msg":"keeping repository directory, other deployments still use the repository"`
		removedLog     = `"msg":"removed unused repository directory"`
		artifactExists = "present"
	)

	h := NewHarness(t, "destroy-shared-repository")
	h.Start()

	h.WaitFor(2*time.Minute, "both stacks deployed", func() bool {
		return h.ComposeContainerID(stackA, service) != "" && h.ComposeContainerID(stackB, service) != ""
	})

	containerA := h.ComposeContainerID(stackA, service)
	if got := h.ExecOutput(containerA, "cat", "/etc/a.conf"); got != "a-v1" {
		t.Fatalf("bind-mounted file = %q, want %q", got, "a-v1")
	}

	revisionA := h.ContainerLabels(containerA)[docker.DocoCDLabels.Deployment.CommitSHA]
	if revisionA == "" {
		t.Fatalf("container of %s has no %s label", stackA, docker.DocoCDLabels.Deployment.CommitSHA)
	}

	waitForRemoval := func(stack string) {
		h.WaitFor(2*time.Minute, stack+" destroyed", func() bool {
			return h.ComposeContainerID(stack, service) == ""
		})
	}

	artifactA := repositoryDir + "/artifacts/" + revisionA
	artifactPresent := func() bool {
		output := h.inspectDataVolume("test -d " + artifactA + " && echo " + artifactExists)
		return strings.TrimSpace(output) == artifactExists
	}

	// Destroy b with the default destroy options, which include remove_dir.
	mark := h.LogMark()
	h.ReplaceInWorktree(".doco-cd.yml", "working_dir: b", "working_dir: b\ndestroy: true")
	h.RepoPush("destroy stack b")

	waitForRemoval(stackB)
	h.WaitForLogAfter(keptLog, mark, 2*time.Minute)

	namesStackA := false

	for line := range strings.SplitSeq(h.logsSince(mark), "\n") {
		if strings.Contains(line, keptLog) && strings.Contains(line, stackA) {
			namesStackA = true
			break
		}
	}

	if !namesStackA {
		t.Fatalf("log line %s does not name %s as a user of the repository", keptLog, stackA)
	}

	if !artifactPresent() {
		t.Fatalf("artifact %s of %s was removed by the destroy of %s", revisionA, stackA, stackB)
	}

	// Stack a must still start, which needs the bind mount source in its artifact.
	containerA = h.ComposeContainerID(stackA, service)

	timeout := 1
	if _, err := h.docker.ContainerRestart(h.ctx, containerA, client.ContainerRestartOptions{Timeout: &timeout}); err != nil {
		t.Fatalf("restart %s/%s: %v", stackA, service, err)
	}

	h.WaitFor(30*time.Second, stackA+"/"+service+" running after restart", func() bool {
		return h.ContainerRunning(containerA)
	})

	if got := h.ExecOutput(containerA, "cat", "/etc/a.conf"); got != "a-v1" {
		t.Fatalf("bind-mounted file after restart = %q, want %q", got, "a-v1")
	}

	// Destroy a as well. No deployment uses the repository anymore, so its directory is removed.
	mark = h.LogMark()
	h.ReplaceInWorktree(".doco-cd.yml", "working_dir: a", "working_dir: a\ndestroy: true")
	h.RepoPush("destroy stack a")

	waitForRemoval(stackA)
	h.WaitForLogAfter(removedLog, mark, 2*time.Minute)

	// The next poll clones the repository again, but only publishes the current revision.
	if artifactPresent() {
		t.Fatalf("artifact %s is still present after the last stack was destroyed", revisionA)
	}
}
