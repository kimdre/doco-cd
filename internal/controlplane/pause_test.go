package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"
)

func pausePollRunner(outcomes map[string]error) PollRunner {
	return func(_ context.Context, cfg poll.Config, _ *app.Config, _ container.MountPoint,
		_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata,
		_ secretprovider.SecretProvider, _ string,
	) error {
		return outcomes[cfg.SourceUrl]
	}
}

func TestPausePollRunClassification(t *testing.T) {
	const secondURL = "https://github.com/kimdre/doco-cd.git"

	paused := stages.NewSkippedRunError([]stages.SkippedStack{
		stages.NewSkippedStack("web", "", stages.DeploymentPaused()),
	})
	retry := &stages.RetrySuppressedError{Stacks: []stages.RetrySuppressedStack{{Name: "api", Reason: "budget_exhausted"}}}
	blocked := &stages.SyncWindowBlockedError{Stacks: []string{"web"}, Windows: []string{"freeze"}}

	failure := errors.New("real failure")
	for _, tc := range []struct {
		name   string
		second error
		status deploymentRunStatus
	}{
		{name: "all paused", second: paused, status: deploymentRunStatusSkipped},
		{name: "partial pause", status: deploymentRunStatusSucceeded},
		{name: "pause and sync window", second: blocked, status: deploymentRunStatusSkipped},
		{name: "pause and retry budget", second: retry, status: deploymentRunStatusSkipped},
		{name: "pause and retry budget in one run", second: errors.Join(retry, paused), status: deploymentRunStatusSkipped},
		{name: "pause retry and sync window in one run", second: errors.Join(retry, blocked, paused), status: deploymentRunStatusSkipped},
		{name: "real failure wins", second: failure, status: deploymentRunStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := newDeploymentRunTracker(nil)
			log := logger.New(logger.LevelCritical)
			runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				appConfig: &app.Config{}, log: log, tracker: tracker,
				pollRunner: pausePollRunner(map[string]error{validPollSourceURL: paused, secondURL: tc.second}),
			})

			id, err := runs.TriggerPoll(t.Context(), []poll.Config{
				{SourceUrl: validPollSourceURL}, {SourceUrl: secondURL},
			}, true, log.Logger)

			wantPause := tc.status == deploymentRunStatusSkipped
			if errors.Is(err, stages.ErrDeploymentPaused) != wantPause {
				t.Fatalf("TriggerPoll() = %v, want pause=%v", err, wantPause)
			}

			if (err != nil) != (tc.status != deploymentRunStatusSucceeded) {
				t.Fatalf("unexpected error: %v", err)
			}

			for _, reason := range []error{stages.ErrRetrySuppressed, stages.ErrSyncWindowBlocked} {
				if wantPause && errors.Is(tc.second, reason) && !errors.Is(err, reason) {
					t.Errorf("skip reason %v lost: %v", reason, err)
				}
			}

			if err != nil {
				if retryText := retry.Error(); strings.Count(err.Error(), retryText) > 1 {
					t.Fatalf("retry outcome is reported more than once: %v", err)
				}
			}

			run, ok := tracker.Get(id)
			if !ok || run.Status != tc.status {
				t.Fatalf("run = %+v, want %s", run, tc.status)
			}
		})
	}
}

func TestPauseConfiguredPollIsSkippedNotFailed(t *testing.T) {
	tracker := newDeploymentRunTracker(nil)
	log := logger.New(logger.LevelCritical)
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig: &app.Config{}, log: log, tracker: tracker,
		pollRunner: pausePollRunner(map[string]error{validPollSourceURL: stages.DeploymentPaused()}),
	})

	id, err := runs.RunConfiguredPoll(t.Context(), poll.Config{SourceUrl: validPollSourceURL}, log.Logger, "poll")
	if err != nil {
		t.Fatalf("automatic paused run failed: %v", err)
	}

	run, ok := tracker.Get(id)
	if !ok || run.Status != deploymentRunStatusSkipped {
		t.Fatalf("run = %+v, want skipped", run)
	}
}
