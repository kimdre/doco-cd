//go:build e2e

package e2e

import (
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

const (
	stackLockBusyStack = "e2e-stack-lock-busy"
	stackLockFreeStack = "e2e-stack-lock-free"
	stackLockedLog     = "stack is locked by another operation, skipping deployment"
)

// TestStackLock_PollSkipsStackHeldByOneOffJob covers issue #2211. A one_off
// job holds the lock of its stack for the whole run. Polls must skip that
// stack and continue to deploy other stacks. After the job ends, a later poll
// must deploy the change that it skipped.
func TestStackLock_PollSkipsStackHeldByOneOffJob(t *testing.T) {
	requireStandaloneSuite(t)
	t.Parallel()

	h := NewHarness(t, "stack-lock-poll-skip")
	h.EnableRemoteContext()
	h.EnableAPI("e2e-stack-lock-api-key")
	h.SetPollInterval(10 * time.Second)
	h.Start()

	var busyAppID, freeAppID string

	h.WaitFor(2*time.Minute, "initial containers of both stacks", func() bool {
		busyAppID = h.RemoteComposeContainerID(stackLockBusyStack, scheduledOneOffAppService)
		freeAppID = h.RemoteComposeContainerID(stackLockFreeStack, scheduledOneOffAppService)

		return busyAppID != "" && freeAppID != ""
	})

	job := getScheduledOneOffJob(t, h, "remote", stackLockBusyStack)

	setGate := scheduledOneOffCompletionGate(t, h, busyAppID)
	t.Cleanup(func() {
		if err := setGate("touch /state/complete"); err != nil {
			t.Errorf("release job completion gate during cleanup: %v", err)
		}
	})

	if err := setGate("rm -f /state/complete && touch /state/ready"); err != nil {
		t.Fatalf("prepare job completion gate: %v", err)
	}

	query := url.Values{"context": {"remote"}, "stack": {stackLockBusyStack}, "wait": {"false"}}
	h.APIRequest(http.MethodPost, "/v1/api/job/"+url.PathEscape(job.Name)+"/run?"+query.Encode(), http.StatusAccepted)

	oneOffID := waitForRunningScheduledOneOff(t, h, stackLockBusyStack)

	// Change both stacks while the job holds the lock of the busy stack.
	mark := h.LogMark()

	h.ReplaceInWorktree("busy/compose.yaml", `REVISION: "1"`, `REVISION: "2"`)
	h.ReplaceInWorktree("free/compose.yaml", `REVISION: "1"`, `REVISION: "2"`)
	h.RepoPush("e2e: change both stacks while the one-off job runs")

	h.WaitForLogAfter(stackLockedLog, mark, time.Minute)
	freeAppID = waitForStackLockRevision(t, h, stackLockFreeStack, freeAppID, "2")

	// A second change shows that later polls still run while the job holds the lock.
	h.ReplaceInWorktree("free/compose.yaml", `REVISION: "2"`, `REVISION: "3"`)
	h.RepoPush("e2e: change the free stack again while the one-off job runs")
	waitForStackLockRevision(t, h, stackLockFreeStack, freeAppID, "3")

	if h.RemoteOneOffContainerID(stackLockBusyStack, scheduledOneOffJobService) != oneOffID || !h.RemoteContainerRunning(oneOffID) {
		t.Fatal("one-off job ended before the free stack deployments completed")
	}

	if err := setGate("touch /state/complete"); err != nil {
		t.Fatalf("release job completion gate: %v", err)
	}

	h.WaitFor(time.Minute, "one-off job completed and cleaned up", func() bool {
		return h.RemoteOneOffContainerID(stackLockBusyStack, scheduledOneOffJobService) == ""
	})

	// The job restores the original app container. A later poll then deploys
	// the change that it skipped while the job ran.
	waitForStackLockRevision(t, h, stackLockBusyStack, busyAppID, "2")
}

// waitForStackLockRevision waits until the app container of stack on the
// remote context is replaced by a container with REVISION=revision. It returns
// the ID of the new container.
func waitForStackLockRevision(t *testing.T, h *Harness, stack, oldID, revision string) string {
	t.Helper()

	var newID string

	h.WaitFor(90*time.Second, stack+" app deployed with REVISION="+revision, func() bool {
		newID = h.RemoteComposeContainerID(stack, scheduledOneOffAppService)
		if newID == "" || newID == oldID {
			return false
		}

		inspect, err := h.remoteDockerClient().ContainerInspect(h.ctx, newID, client.ContainerInspectOptions{})
		if err != nil || inspect.Container.Config == nil {
			return false
		}

		return slices.Contains(inspect.Container.Config.Env, "REVISION="+revision)
	})

	return newID
}
