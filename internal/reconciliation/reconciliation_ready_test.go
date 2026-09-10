package reconciliation

import (
	"testing"
	"time"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// TestRunContextEventListener_SignalsReadyWithoutReconciliationConfigs ensures a context/mode
// group that has deploy configs but none with reconciliation enabled still reports readiness.
// run counts one listener per non-empty context/mode group, so a silent listener would stall
// the job's readiness signal forever and block callers waiting for it (startup state recovery).
func TestRunContextEventListener_SignalsReadyWithoutReconciliationConfigs(t *testing.T) {
	t.Parallel()

	disabled := deployConfig.New("no-reconciliation", "main")
	disabled.Reconciliation.Enabled = false

	j := newJob(nil, DeployRequest{
		Logger:     logger.New(logger.LevelCritical).Logger,
		JobTrigger: stages.JobTriggerPoll,
		Payload:    &webhook.ParsedPayload{FullName: "owner/repo"},
	}, nil)

	ready := make(chan struct{}, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)

		j.runContextEventListener(t.Context(), j.info.Logger, "", contextCLIEntry{}, false,
			[]*deployConfig.Config{disabled}, nil, ready)
	}()

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not signal readiness for a context without reconciliation-enabled configs")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not return after signalling readiness")
	}
}

// TestRunContextEventListener_ReadySignalDoesNotBlockOnClose ensures the readiness signal is
// abandoned when the job is closed before the readiness collector drained the channel, so the
// listener goroutine cannot leak and block the job from shutting down.
func TestRunContextEventListener_ReadySignalDoesNotBlockOnClose(t *testing.T) {
	t.Parallel()

	disabled := deployConfig.New("no-reconciliation", "main")
	disabled.Reconciliation.Enabled = false

	j := newJob(nil, DeployRequest{
		Logger:     logger.New(logger.LevelCritical).Logger,
		JobTrigger: stages.JobTriggerPoll,
		Payload:    &webhook.ParsedPayload{FullName: "owner/repo"},
	}, nil)

	j.close()

	// Unbuffered: nobody reads it, so a blocking send would hang the listener forever.
	ready := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		j.runContextEventListener(t.Context(), j.info.Logger, "", contextCLIEntry{}, false,
			[]*deployConfig.Config{disabled}, nil, ready)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("listener blocked on the readiness signal after the job was closed")
	}
}
