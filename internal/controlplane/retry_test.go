package controlplane

import (
	"context"
	"errors"
	"log/slog"
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

func TestConfiguredPollRecordsSuppressedRetryAsSkipped(t *testing.T) {
	t.Parallel()

	suppressed := &stages.RetrySuppressedError{Stacks: []stages.RetrySuppressedStack{{
		Name: "web", Reason: "budget_exhausted",
	}}}
	log := logger.New(logger.LevelCritical)
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig: &app.Config{},
		log:       log,
		pollRunner: func(context.Context, poll.Config, *app.Config, container.MountPoint,
			command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, secretprovider.SecretProvider, string,
		) error {
			return suppressed
		},
	})

	jobID, err := runs.RunConfiguredPoll(t.Context(), poll.Config{SourceUrl: validPollSourceURL}, log.Logger, "poll")
	if err != nil {
		t.Fatal(err)
	}

	run, ok := runs.Get(jobID)
	if !ok || run.Status != RunStatusSkipped || run.Message != suppressed.Error() {
		t.Fatalf("run = %#v, found = %t", run, ok)
	}
}

func TestTriggerPollPreservesSuppressionAndRealFailures(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		sibling error
		joined  bool
		want    RunStatus
	}{
		{name: "suppressed", want: RunStatusSkipped},
		{name: "successful sibling", want: RunStatusSkipped},
		{name: "deferred sibling", sibling: &stages.SyncWindowBlockedError{Stacks: []string{"db"}, Windows: []string{"freeze"}}, want: RunStatusSkipped},
		{name: "nested deferral", sibling: &stages.SyncWindowBlockedError{Stacks: []string{"db"}, Windows: []string{"freeze"}}, joined: true, want: RunStatusSkipped},
		{name: "failed sibling", sibling: errors.New("real failure"), want: RunStatusFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			suppressed := &stages.RetrySuppressedError{Stacks: []stages.RetrySuppressedStack{{
				Name: "web", Reason: "budget_exhausted",
			}}}
			log := logger.New(logger.LevelCritical)
			runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				appConfig: &app.Config{},
				log:       log,
				pollRunner: func(_ context.Context, cfg poll.Config, _ *app.Config, _ container.MountPoint,
					_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider, _ string,
				) error {
					if cfg.CustomTarget == "web" {
						if tt.joined {
							return errors.Join(suppressed, tt.sibling)
						}

						return suppressed
					}

					return tt.sibling
				},
			})

			configs := []poll.Config{{SourceUrl: validPollSourceURL, CustomTarget: "web"}}
			if tt.name != "suppressed" && !tt.joined {
				configs = append(configs, poll.Config{SourceUrl: validPollSourceURL, CustomTarget: "db"})
			}

			jobID, err := runs.TriggerPoll(t.Context(), configs, true, log.Logger)

			run, ok := runs.Get(jobID)
			if !ok || run.Status != tt.want {
				t.Fatalf("run = %#v, found = %t, want %s", run, ok, tt.want)
			}

			if tt.want == RunStatusSkipped && !errors.Is(err, stages.ErrRetrySuppressed) {
				t.Fatalf("suppression identity lost: %v", err)
			}

			if errors.Is(tt.sibling, stages.ErrSyncWindowBlocked) && !errors.Is(err, stages.ErrSyncWindowBlocked) {
				t.Fatalf("nested sync-window deferral lost: %v", err)
			}

			if tt.want == RunStatusFailed {
				if _, ok := errors.AsType[*PollRunsFailedError](err); !ok || errors.Is(err, stages.ErrRetrySuppressed) {
					t.Fatalf("real failure misclassified: %v", err)
				}
			}
		})
	}
}
