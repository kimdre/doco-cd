package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

func TestDeploymentOrigin(t *testing.T) {
	t.Parallel()

	if got := DeploymentOrigin(t.Context()); got != syncwindow.OriginAutomatic {
		t.Fatalf("DeploymentOrigin() without origin = %q, want automatic", got)
	}

	ctx := WithDeploymentOrigin(t.Context(), syncwindow.OriginManual)
	if got := DeploymentOrigin(ctx); got != syncwindow.OriginManual {
		t.Fatalf("DeploymentOrigin() = %q, want manual", got)
	}

	if got := DeploymentOrigin(WithDeploymentOrigin(ctx, "")); got != syncwindow.OriginAutomatic {
		t.Fatalf("DeploymentOrigin() with empty origin = %q, want automatic", got)
	}
}

// syncWindowPollRunner returns a poll runner that defers the configs whose
// source URL is in deferred and records the origin of every run.
func syncWindowPollRunner(deferred map[string]bool, origins *[]syncwindow.Origin) PollRunner {
	var mu sync.Mutex

	return func(ctx context.Context, cfg poll.Config, _ *app.Config, _ container.MountPoint,
		_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider, _ string,
	) error {
		mu.Lock()

		*origins = append(*origins, DeploymentOrigin(ctx))
		mu.Unlock()

		if deferred[cfg.SourceUrl] {
			return &stages.SyncWindowBlockedError{
				Stacks:   []string{"web"},
				Windows:  []string{"freeze"},
				NextOpen: time.Date(2026, time.March, 10, 11, 0, 0, 0, time.UTC),
			}
		}

		return nil
	}
}

func TestControlPlaneRunsTriggerPollReportsSyncWindowDeferral(t *testing.T) {
	const otherPollSourceURL = "https://github.com/kimdre/doco-cd.git"

	for _, testCase := range []struct {
		name        string
		deferred    map[string]bool
		wantStatus  deploymentRunStatus
		wantMessage string
		wantBlocked bool
	}{
		{
			name:        "all deferred",
			deferred:    map[string]bool{validPollSourceURL: true, otherPollSourceURL: true},
			wantStatus:  deploymentRunStatusSkipped,
			wantMessage: "deployment of web deferred by sync window freeze until 2026-03-10T11:00:00Z",
			wantBlocked: true,
		},
		{
			name:        "partly deferred",
			deferred:    map[string]bool{validPollSourceURL: true},
			wantStatus:  deploymentRunStatusSucceeded,
			wantMessage: "poll jobs complete",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var origins []syncwindow.Origin

			tracker := newDeploymentRunTracker(nil)
			log := logger.New(logger.LevelCritical)
			controlPlane := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				appConfig:  &app.Config{},
				log:        log,
				tracker:    tracker,
				pollRunner: syncWindowPollRunner(testCase.deferred, &origins),
			})

			jobID, err := controlPlane.TriggerPoll(t.Context(),
				[]poll.Config{{SourceUrl: validPollSourceURL}, {SourceUrl: otherPollSourceURL}}, true, log.Logger)

			if got := errors.Is(err, stages.ErrSyncWindowBlocked); got != testCase.wantBlocked {
				t.Fatalf("TriggerPoll() error = %v, want blocked = %v", err, testCase.wantBlocked)
			}

			if !testCase.wantBlocked && err != nil {
				t.Fatalf("TriggerPoll() error = %v, want nil", err)
			}

			run, ok := tracker.Get(jobID)
			if !ok {
				t.Fatal("tracked run not found")
			}

			if run.Status != testCase.wantStatus || run.Message != testCase.wantMessage {
				t.Fatalf("tracked run = %#v", run)
			}

			// API and MCP triggered polls are manual deployments.
			if len(origins) != 2 || origins[0] != syncwindow.OriginManual || origins[1] != syncwindow.OriginManual {
				t.Fatalf("poll runs had origins %v, want manual", origins)
			}
		})
	}
}

func TestControlPlaneRunsRunConfiguredPollReportsSyncWindowDeferral(t *testing.T) {
	var origins []syncwindow.Origin

	tracker := newDeploymentRunTracker(nil)
	log := logger.New(logger.LevelCritical)
	controlPlane := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig:  &app.Config{},
		log:        log,
		tracker:    tracker,
		pollRunner: syncWindowPollRunner(map[string]bool{validPollSourceURL: true}, &origins),
	})

	jobID, err := controlPlane.RunConfiguredPoll(t.Context(), poll.Config{SourceUrl: validPollSourceURL}, log.Logger, "poll")
	if err != nil {
		t.Fatalf("RunConfiguredPoll() error = %v, want nil for a deferred poll", err)
	}

	run, ok := tracker.Get(jobID)
	if !ok {
		t.Fatal("tracked run not found")
	}

	if run.Status != deploymentRunStatusSkipped || run.Message != "deployment of web deferred by sync window freeze until 2026-03-10T11:00:00Z" {
		t.Fatalf("tracked run = %#v", run)
	}

	// Configured polls are automatic deployments.
	if len(origins) != 1 || origins[0] != syncwindow.OriginAutomatic {
		t.Fatalf("poll run had origins %v, want automatic", origins)
	}
}
