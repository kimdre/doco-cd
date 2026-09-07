//go:build e2e

package e2e

import (
	"testing"
	"time"
)

const (
	scheduledOneOffStack              = "e2e-scheduled-one-off"
	scheduledOneOffRecoveryStack      = "e2e-scheduled-one-off-recovery"
	scheduledOneOffSwarmRecoveryStack = "e2e-scheduled-one-off-swarm-recovery"
	scheduledOneOffJobService         = "backup"
	scheduledOneOffAppService         = "app"
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

func TestScheduledOneOff_SwarmRecoversAfterForcedDaemonTermination(t *testing.T) {
	t.Parallel()

	h := NewHarness(t, "scheduled-one-off-swarm-recovery")
	if !h.isSwarmMode() {
		t.Skip("scheduled one-off Swarm recovery requires a Swarm manager")
	}

	h.SetPollInterval(time.Minute)
	h.Start()

	h.WaitFor(2*time.Minute, "initial Swarm dependency service", func() bool {
		return h.SwarmContainerID(scheduledOneOffSwarmRecoveryStack, scheduledOneOffAppService) != ""
	})

	oneOffID := waitForRunningSwarmScheduledOneOff(t, h, scheduledOneOffSwarmRecoveryStack)

	h.WaitFor(30*time.Second, "Swarm dependency stopped before daemon termination", func() bool {
		return h.SwarmContainerID(scheduledOneOffSwarmRecoveryStack, scheduledOneOffAppService) == ""
	})

	h.KillAndRestartDaemon()

	h.WaitFor(30*time.Second, "replacement daemon reuses running Swarm one-off service", func() bool {
		return h.SwarmOneOffServiceID(scheduledOneOffSwarmRecoveryStack) == oneOffID &&
			h.SwarmServiceHasRunningTask(oneOffID)
	})

	h.WaitFor(2*time.Minute, "recovered Swarm one-off service cleanup", func() bool {
		return h.SwarmOneOffServiceID(scheduledOneOffSwarmRecoveryStack) == ""
	})

	h.WaitFor(30*time.Second, "Swarm dependency restored by recovered one-off job", func() bool {
		return h.SwarmContainerID(scheduledOneOffSwarmRecoveryStack, scheduledOneOffAppService) != ""
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

func waitForRunningSwarmScheduledOneOff(t *testing.T, h *Harness, stack string) string {
	t.Helper()

	var oneOffID string

	h.WaitFor(2*time.Minute, "running scheduled Swarm one-off job", func() bool {
		oneOffID = h.SwarmOneOffServiceID(stack)
		return oneOffID != "" && h.SwarmServiceHasRunningTask(oneOffID)
	})

	return oneOffID
}
