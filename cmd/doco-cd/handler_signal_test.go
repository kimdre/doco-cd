package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/stages"
)

// signalTriggerCall records one call of a fake pollSignalTrigger.
type signalTriggerCall struct {
	configs []poll.Config
	reason  string
}

// fakeSignalTrigger records trigger calls and returns the configured errors in order.
type fakeSignalTrigger struct {
	mu     sync.Mutex
	calls  []signalTriggerCall
	errs   []error
	called chan struct{}
}

func newFakeSignalTrigger(errs ...error) *fakeSignalTrigger {
	return &fakeSignalTrigger{
		errs:   errs,
		called: make(chan struct{}, 16),
	}
}

func (f *fakeSignalTrigger) trigger(_ context.Context, configs []poll.Config, triggerReason string, _ *slog.Logger) (string, error) {
	f.mu.Lock()

	// Change the batch like controlplane.Runs.TriggerConfiguredPolls does.
	for i := range configs {
		configs[i].RunOnce = true
		configs[i].Interval = 0
		configs[i].Schedule = ""

		for _, d := range configs[i].Deployments {
			d.Name = "changed"
			d.Environment["KEY"] = "changed"
		}
	}

	f.calls = append(f.calls, signalTriggerCall{configs: configs, reason: triggerReason})

	var err error
	if n := len(f.calls) - 1; n < len(f.errs) {
		err = f.errs[n]
	}

	f.mu.Unlock()

	f.called <- struct{}{}

	return "job-id", err
}

func (f *fakeSignalTrigger) snapshot() []signalTriggerCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]signalTriggerCall(nil), f.calls...)
}

func (f *fakeSignalTrigger) waitForCalls(t *testing.T, n int) {
	t.Helper()

	for range n {
		select {
		case <-f.called:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %d poll trigger calls, got %d", n, len(f.snapshot()))
		}
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// startPollSignalLoop starts the loop and returns a function that stops it and waits for the exit.
func startPollSignalLoop(t *testing.T, sigCh <-chan os.Signal, configs []poll.Config, trigger pollSignalTrigger) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		runPollSignalLoop(ctx, sigCh, configs, trigger, discardLogger())
	}()

	return func() {
		cancel()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("poll signal loop did not stop after context cancellation")
		}
	}
}

func TestPollJobEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config poll.Config
		want   bool
	}{
		{name: "interval", config: poll.Config{SourceUrl: "https://example.com/repo.git", Interval: time.Minute}, want: true},
		{name: "schedule", config: poll.Config{SourceUrl: "https://example.com/repo.git", Schedule: "@hourly"}, want: true},
		{name: "run once", config: poll.Config{SourceUrl: "https://example.com/repo.git", RunOnce: true}, want: true},
		{name: "local git repository", config: poll.Config{Source: "git", SourceUrl: "file:///repo"}, want: true},
		{name: "local git repository with default source", config: poll.Config{SourceUrl: "file:///repo"}, want: true},
		{name: "remote repository with interval zero", config: poll.Config{SourceUrl: "https://example.com/repo.git"}, want: false},
		{name: "oci artifact with interval zero", config: poll.Config{Source: "oci", SourceUrl: "oci://example.com/app:latest"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := pollJobEnabled(tt.config); got != tt.want {
				t.Fatalf("pollJobEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunPollSignalLoopTriggersEnabledConfigs(t *testing.T) {
	t.Parallel()

	configs := []poll.Config{
		{
			SourceUrl: "https://example.com/enabled.git",
			Interval:  time.Minute,
			Deployments: []*deploy.Config{{
				Name:        "web",
				Environment: map[string]string{"KEY": "value"},
			}},
		},
		{SourceUrl: "https://example.com/disabled.git"},
		{SourceUrl: "https://example.com/scheduled.git", Schedule: "@hourly"},
	}

	fake := newFakeSignalTrigger()
	sigCh := make(chan os.Signal, 1)
	stop := startPollSignalLoop(t, sigCh, configs, fake.trigger)

	defer stop()

	sigCh <- syscall.SIGHUP

	fake.waitForCalls(t, 1)

	sigCh <- syscall.SIGUSR1

	fake.waitForCalls(t, 1)

	calls := fake.snapshot()
	if len(calls) != 2 {
		t.Fatalf("expected 2 trigger calls, got %d", len(calls))
	}

	for i, call := range calls {
		if call.reason != pollTriggerSignal {
			t.Fatalf("call %d: trigger reason = %q, want %q", i, call.reason, pollTriggerSignal)
		}

		if len(call.configs) != 2 {
			t.Fatalf("call %d: expected 2 enabled configs, got %d", i, len(call.configs))
		}

		if call.configs[0].SourceUrl != configs[0].SourceUrl || call.configs[1].SourceUrl != configs[2].SourceUrl {
			t.Fatalf("call %d: unexpected batch %q, %q", i, call.configs[0].SourceUrl, call.configs[1].SourceUrl)
		}
	}

	// The trigger changes its batch in place. Each signal must get a fresh copy of the configured values.
	if configs[0].Interval != time.Minute || configs[0].RunOnce || configs[2].Schedule != "@hourly" {
		t.Fatalf("configured poll values changed: %+v, %+v", configs[0], configs[2])
	}

	if configs[0].Deployments[0].Name != "web" || configs[0].Deployments[0].Environment["KEY"] != "value" {
		t.Fatalf("configured deployment changed: %+v", configs[0].Deployments[0])
	}

	if calls[1].configs[0].Deployments[0] == calls[0].configs[0].Deployments[0] {
		t.Fatal("signal batches must not share deployment configs")
	}
}

func TestRunPollSignalLoopWithoutEnabledConfigs(t *testing.T) {
	t.Parallel()

	var logOutput bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelDebug}))
	fake := newFakeSignalTrigger()

	triggerPollsFromSignal(t.Context(), syscall.SIGHUP, nil, fake.trigger, logger)

	if calls := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("expected no trigger calls, got %d", len(calls))
	}

	output := logOutput.String()
	if !strings.Contains(output, "no enabled poll configurations exist") || !strings.Contains(output, "signal=hangup") {
		t.Fatalf("expected a log line for the ignored signal, got %q", output)
	}
}

func TestRunPollSignalLoopContinuesAfterFailure(t *testing.T) {
	t.Parallel()

	configs := []poll.Config{{SourceUrl: "https://example.com/repo.git", Interval: time.Minute}}
	fake := newFakeSignalTrigger(errors.New("deployment failed"))
	sigCh := make(chan os.Signal, 1)
	stop := startPollSignalLoop(t, sigCh, configs, fake.trigger)

	defer stop()

	sigCh <- syscall.SIGHUP

	fake.waitForCalls(t, 1)

	sigCh <- syscall.SIGHUP

	fake.waitForCalls(t, 1)
}

func TestRunPollSignalLoopStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	fake := newFakeSignalTrigger()
	sigCh := make(chan os.Signal, 1)
	stop := startPollSignalLoop(t, sigCh, []poll.Config{{SourceUrl: "https://example.com/repo.git", Interval: time.Minute}}, fake.trigger)

	stop()

	if calls := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("expected no trigger calls, got %d", len(calls))
	}
}

func TestRunPollSignalLoopIgnoresSignalAfterCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	fake := newFakeSignalTrigger()

	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGHUP

	runPollSignalLoop(ctx, sigCh, []poll.Config{{SourceUrl: "https://example.com/repo.git", Interval: time.Minute}}, fake.trigger, discardLogger())

	if calls := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("expected no trigger calls after cancellation, got %d", len(calls))
	}
}

func TestDrainPollSignal(t *testing.T) {
	t.Parallel()

	sigCh := make(chan os.Signal, 1)

	// An empty channel must not block.
	drainPollSignal(sigCh, discardLogger())

	sigCh <- syscall.SIGHUP

	drainPollSignal(sigCh, discardLogger())

	if len(sigCh) != 0 {
		t.Fatal("expected the startup signal to be removed")
	}
}

// TestNotifyPollSignals sends real signals to the test process.
// It must not run in parallel, because process signals are global state.
func TestNotifyPollSignals(t *testing.T) {
	sigCh, stop := notifyPollSignals()
	defer stop()

	if cap(sigCh) != 1 {
		t.Fatalf("signal buffer capacity = %d, want 1", cap(sigCh))
	}

	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGUSR1} {
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			t.Fatalf("failed to send %s: %v", sig, err)
		}

		select {
		case got := <-sigCh:
			if got != sig {
				t.Fatalf("received %s, want %s", got, sig)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("did not receive %s", sig)
		}
	}
}

func TestRunPollSignalLoopCoalescesSignals(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		configs := []poll.Config{{SourceUrl: "https://example.com/repo.git", Interval: time.Minute}}
		sigCh := make(chan os.Signal, 1)
		release := make(chan struct{})
		calls := 0

		stop := startPollSignalLoop(t, sigCh, configs, func(ctx context.Context, _ []poll.Config, _ string, _ *slog.Logger) (string, error) {
			calls++
			if calls == 1 {
				select {
				case <-release:
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}

			return "", nil
		})
		defer stop()

		sigCh <- syscall.SIGHUP

		synctest.Wait()

		for range 3 {
			// os/signal drops a signal when the buffer is full.
			select {
			case sigCh <- syscall.SIGUSR1:
			default:
			}
		}

		synctest.Wait()

		if calls != 1 {
			t.Fatalf("started %d batches before the first finished, want 1", calls)
		}

		close(release)
		synctest.Wait()

		if calls != 2 {
			t.Fatalf("started %d batches after the first finished, want 2", calls)
		}
	})
}

func TestLogPollSignalResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		wantLevel string
	}{
		{name: "success", err: nil, wantLevel: "level=INFO"},
		{name: "paused", err: fmt.Errorf("skip: %w", stages.ErrDeploymentPaused), wantLevel: "level=INFO"},
		{name: "retry suppressed", err: fmt.Errorf("skip: %w", stages.ErrRetrySuppressed), wantLevel: "level=DEBUG"},
		{name: "sync window", err: &stages.SyncWindowBlockedError{Stacks: []string{"web"}, Windows: []string{"freeze"}}, wantLevel: "level=INFO"},
		{name: "lifecycle cancellation", err: &controlplane.PollRunsFailedError{Failed: 1, Total: 1, Cause: context.Canceled}, wantLevel: "level=DEBUG"},
		{name: "background work closed", err: controlplane.ErrBackgroundWorkClosed, wantLevel: "level=DEBUG"},
		{name: "failure", err: &controlplane.PollRunsFailedError{Failed: 1, Total: 1}, wantLevel: "level=ERROR"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logOutput bytes.Buffer

			logger := slog.New(slog.NewTextHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelDebug}))
			logPollSignalResult(logger, tt.err)

			if output := logOutput.String(); !strings.Contains(output, " "+tt.wantLevel+" ") {
				t.Fatalf("expected %s, got %q", tt.wantLevel, output)
			}
		})
	}
}
