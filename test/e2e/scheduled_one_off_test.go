//go:build e2e

package e2e

import (
	"testing"
	"time"
)

const (
	scheduledOneOffStack         = "e2e-scheduled-one-off"
	scheduledOneOffRecoveryStack = "e2e-scheduled-one-off-recovery"
	scheduledOneOffJobService    = "backup"
	scheduledOneOffAppService    = "app"
)

func TestScheduledOneOff(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "scheduled-one-off")
	h.EnableRemoteContext()
	h.SetPollInterval(time.Minute)
	h.Start()

	h.WaitFor(2*time.Minute, "initial dependency container", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffStack, scheduledOneOffAppService) != ""
	})

	waitForRunningScheduledOneOff(t, h, scheduledOneOffStack)

	h.WaitFor(30*time.Second, "dependency stopped while one-off job runs", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffStack, scheduledOneOffAppService) == ""
	})

	h.WaitFor(2*time.Minute, "one-off job artifact cleanup", func() bool {
		return h.RemoteOneOffContainerID(scheduledOneOffStack, scheduledOneOffJobService) == ""
	})

	h.WaitFor(30*time.Second, "dependency restored after one-off job", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffStack, scheduledOneOffAppService) != ""
	})
}

func TestScheduledOneOff_RecoversAfterForcedDaemonTermination(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "scheduled-one-off-recovery")
	h.EnableRemoteContext()
	h.SetPollInterval(time.Minute)
	h.Start()

	h.WaitFor(2*time.Minute, "initial dependency container", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffRecoveryStack, scheduledOneOffAppService) != ""
	})

	oneOffID := waitForRunningScheduledOneOff(t, h, scheduledOneOffRecoveryStack)

	h.WaitFor(30*time.Second, "dependency stopped before daemon termination", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffRecoveryStack, scheduledOneOffAppService) == ""
	})

	h.KillAndRestartDaemon()

	h.WaitFor(30*time.Second, "replacement daemon retains running one-off job", func() bool {
		return h.RemoteOneOffContainerID(scheduledOneOffRecoveryStack, scheduledOneOffJobService) == oneOffID &&
			h.RemoteContainerRunning(oneOffID)
	})

	h.WaitFor(2*time.Minute, "recovered one-off job artifact cleanup", func() bool {
		return h.RemoteOneOffContainerID(scheduledOneOffRecoveryStack, scheduledOneOffJobService) == ""
	})

	h.WaitFor(30*time.Second, "dependency restored by recovered one-off job", func() bool {
		return h.RemoteComposeContainerID(scheduledOneOffRecoveryStack, scheduledOneOffAppService) != ""
	})
}

func waitForRunningScheduledOneOff(t *testing.T, h *Harness, stack string) string {
	t.Helper()

	var oneOffID string

	h.WaitFor(2*time.Minute, "running scheduled one-off job", func() bool {
		oneOffID = h.RemoteOneOffContainerID(stack, scheduledOneOffJobService)
		return oneOffID != "" && h.RemoteContainerRunning(oneOffID)
	})

	return oneOffID
}
