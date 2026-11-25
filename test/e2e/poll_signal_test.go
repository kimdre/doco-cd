//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// TestPollSignalTrigger checks that SIGHUP and SIGUSR1 start a poll run (#2242).
// The poll interval is long, so only the signal can deploy a new commit.
// The daemon must keep running after each signal.
func TestPollSignalTrigger(t *testing.T) {
	requireStandaloneSuite(t)
	t.Parallel()

	const stack = "e2e-poll-signal"

	h := NewHarness(t, "poll-signal")
	h.SetPollInterval(24 * time.Hour)
	h.Start()
	// The startup poll holds the stack lock until the job ends. A signal poll
	// during that time skips the busy stack, so wait for the job to end first.
	h.WaitForLog("job completed successfully", 2*time.Minute)

	containerID := func() string { return h.ComposeContainerID(stack, "app") }
	daemonID := h.daemon.GetContainerID()

	steps := []struct {
		signal  string
		from    string
		version string
	}{
		{signal: "SIGHUP", from: `VERSION: "1"`, version: "2"},
		{signal: "SIGUSR1", from: `VERSION: "2"`, version: "3"},
	}

	for _, step := range steps {
		oldID := containerID()
		if oldID == "" {
			t.Fatalf("%s app container is missing before %s", stack, step.signal)
		}

		h.ReplaceInWorktree("deploy/compose.yaml", step.from, `VERSION: "`+step.version+`"`)
		h.RepoPush("update app to version " + step.version)

		h.AssertStays(5*time.Second, "new commit is not deployed without a poll", oldID, containerID)

		mark := h.LogMark()
		h.SignalDaemon(step.signal)

		h.WaitForLogAfter(`"event":"poll-signal"`, mark, time.Minute)
		h.WaitForComposeContainerRecreate(stack, "app", oldID, 2*time.Minute)
		h.WaitForLogAfter("poll run triggered by signal complete", mark, 2*time.Minute)

		if got := h.ExecOutput(containerID(), "printenv", "VERSION"); got != step.version {
			t.Fatalf("VERSION after %s = %q, want %q", step.signal, got, step.version)
		}

		if !h.ContainerRunning(daemonID) {
			t.Fatalf("doco-cd daemon stopped after %s", step.signal)
		}
	}
}
