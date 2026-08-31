package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

const validPollSourceURL = "https://github.com/kimdre/doco-cd_tests.git"

func TestControlPlaneRunsTriggerPollAppliesDefaultsAndReportsIndexedValidationErrors(t *testing.T) {
	t.Run("empty configs", func(t *testing.T) {
		tracker := newDeploymentRunTracker(nil)
		runCount := 0
		log := logger.New(logger.LevelCritical)
		controlPlane := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			appConfig: &app.Config{},
			log:       log,
			tracker:   tracker,
			pollRunner: func(context.Context, poll.Config, *app.Config, container.MountPoint,
				command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, secretprovider.SecretProvider, string,
			) error {
				runCount++

				return nil
			},
		})

		jobID, err := controlPlane.TriggerPoll(t.Context(), nil, true, log.Logger)
		if !errors.Is(err, ErrNoPollConfiguration) {
			t.Fatalf("empty configs error = %v", err)
		}

		if jobID == "" {
			t.Fatal("expected generated job ID")
		}

		if _, ok := tracker.Get(jobID); ok {
			t.Fatal("empty configs must not be tracked")
		}

		if runCount != 0 {
			t.Fatalf("empty configs started %d poll runs", runCount)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		tracker := newDeploymentRunTracker(nil)

		var (
			gotConfig   poll.Config
			gotMetadata notification.Metadata
		)

		log := logger.New(logger.LevelCritical)
		controlPlane := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			appConfig: &app.Config{},
			log:       log,
			tracker:   tracker,
			pollRunner: func(_ context.Context, cfg poll.Config, _ *app.Config, _ container.MountPoint,
				_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, metadata notification.Metadata, _ secretprovider.SecretProvider, _ string,
			) error {
				gotConfig = cfg
				gotMetadata = metadata

				return nil
			},
		})

		jobID, err := controlPlane.TriggerPoll(t.Context(), []poll.Config{{
			SourceUrl:    validPollSourceURL,
			Interval:     time.Hour,
			CustomTarget: "prod-vm",
		}}, true, log.Logger)
		if err != nil {
			t.Fatal(err)
		}

		if jobID == "" {
			t.Fatal("expected generated job ID")
		}

		if !gotConfig.RunOnce || gotConfig.Interval != 0 {
			t.Fatalf("poll defaults = run_once %t, interval %s", gotConfig.RunOnce, gotConfig.Interval)
		}

		if gotMetadata.Target != "prod-vm" {
			t.Fatalf("poll metadata target = %q, want %q", gotMetadata.Target, "prod-vm")
		}

		run, ok := tracker.Get(jobID)
		if !ok {
			t.Fatal("tracked poll run not found")
		}

		if run.Target != "prod-vm" {
			t.Fatalf("tracked target = %q, want %q", run.Target, "prod-vm")
		}
	})

	t.Run("indexed validation", func(t *testing.T) {
		tracker := newDeploymentRunTracker(nil)
		runCount := 0
		log := logger.New(logger.LevelCritical)
		controlPlane := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			appConfig: &app.Config{},
			log:       log,
			tracker:   tracker,
			pollRunner: func(context.Context, poll.Config, *app.Config, container.MountPoint,
				command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, secretprovider.SecretProvider, string,
			) error {
				runCount++

				return nil
			},
		})

		jobID, err := controlPlane.TriggerPoll(t.Context(), []poll.Config{{SourceUrl: validPollSourceURL}, {}}, true, log.Logger)
		if !errors.Is(err, deploy.ErrKeyNotFound) || !strings.Contains(err.Error(), "at index 1:") {
			t.Fatalf("unexpected validation error: %v", err)
		}

		if jobID == "" {
			t.Fatal("expected job ID for validation response")
		}

		if runCount != 0 {
			t.Fatalf("validation failure started %d poll runs", runCount)
		}

		if _, ok := tracker.Get(jobID); ok {
			t.Fatal("validation failure must not be tracked")
		}
	})
}

func TestControlPlaneRunsTriggerPollRejectsTooManyConfigs(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(nil)
	log := logger.New(logger.LevelCritical)
	controlPlane := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig: &app.Config{},
		log:       log,
		tracker:   tracker,
	})

	configs := make([]poll.Config, MaxTriggerPollConfigs+1)
	for i := range configs {
		configs[i].SourceUrl = validPollSourceURL
	}

	jobID, err := controlPlane.TriggerPoll(t.Context(), configs, true, log.Logger)
	if !errors.Is(err, ErrTooManyPollConfigurations) {
		t.Fatalf("error = %v, want %v", err, ErrTooManyPollConfigurations)
	}

	if _, ok := tracker.Get(jobID); ok {
		t.Fatal("rejected poll batch must not be tracked")
	}
}

func TestControlPlaneRunsTriggerPollBoundsConcurrentRunners(t *testing.T) {
	t.Parallel()

	const maxConcurrent = 2

	var (
		running atomic.Int32
		peak    atomic.Int32
	)

	release := make(chan struct{})
	started := make(chan struct{}, MaxTriggerPollConfigs)
	log := logger.New(logger.LevelCritical)
	controlPlane := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig: &app.Config{MaxConcurrentDeployments: maxConcurrent},
		log:       log,
		pollRunner: func(context.Context, poll.Config, *app.Config, container.MountPoint,
			command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, secretprovider.SecretProvider, string,
		) error {
			current := running.Add(1)
			for previous := peak.Load(); current > previous; previous = peak.Load() {
				if peak.CompareAndSwap(previous, current) {
					break
				}
			}

			started <- struct{}{}

			<-release
			running.Add(-1)

			return nil
		},
	})

	configs := make([]poll.Config, 5)
	for i := range configs {
		configs[i].SourceUrl = validPollSourceURL
	}

	done := make(chan error, 1)

	go func() {
		_, err := controlPlane.TriggerPoll(t.Context(), configs, true, log.Logger)
		done <- err
	}()

	for range maxConcurrent {
		<-started
	}

	select {
	case <-started:
		t.Fatal("poll runners exceeded configured concurrency")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if got := peak.Load(); got != maxConcurrent {
		t.Fatalf("peak concurrent poll runners = %d, want %d", got, maxConcurrent)
	}
}

func TestControlPlaneRunsTriggerPollTracksSuccessFailureAndPanic(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		run         PollRunner
		wantStatus  deploymentRunStatus
		wantMessage string
		wantError   bool
	}{
		{
			name: "success",
			run: func(context.Context, poll.Config, *app.Config, container.MountPoint,
				command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, secretprovider.SecretProvider, string,
			) error {
				return nil
			},
			wantStatus:  deploymentRunStatusSucceeded,
			wantMessage: "poll jobs complete",
		},
		{
			name: "failure",
			run: func(context.Context, poll.Config, *app.Config, container.MountPoint,
				command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, secretprovider.SecretProvider, string,
			) error {
				return errors.New("secret failure")
			},
			wantStatus:  deploymentRunStatusFailed,
			wantMessage: "1/1 poll jobs failed",
			wantError:   true,
		},
		{
			name: "panic",
			run: func(context.Context, poll.Config, *app.Config, container.MountPoint,
				command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, secretprovider.SecretProvider, string,
			) error {
				panic("sensitive panic value")
			},
			wantStatus:  deploymentRunStatusFailed,
			wantMessage: "1/1 poll jobs failed",
			wantError:   true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tracker := newDeploymentRunTracker(nil)
			log := logger.New(logger.LevelCritical)
			controlPlane := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				appConfig:  &app.Config{},
				log:        log,
				tracker:    tracker,
				pollRunner: testCase.run,
			})

			jobID, err := controlPlane.TriggerPoll(t.Context(), []poll.Config{{SourceUrl: validPollSourceURL}}, true, log.Logger)
			if (err != nil) != testCase.wantError {
				t.Fatalf("run error = %v, wantError %t", err, testCase.wantError)
			}

			run, ok := tracker.Get(jobID)
			if !ok {
				t.Fatal("tracked run not found")
			}

			if run.Status != testCase.wantStatus || run.Message != testCase.wantMessage {
				t.Fatalf("tracked run = %#v", run)
			}

			if run.Repository != git.GetRepoName(validPollSourceURL) {
				t.Fatalf("repository metadata = %q", run.Repository)
			}
		})
	}
}

func TestControlPlaneRunsTriggerPollAsyncUsesApplicationLifecycle(t *testing.T) {
	appCtx, cancelApp := context.WithCancel(t.Context())
	started := make(chan struct{})
	finished := make(chan error, 1)
	tracker := newDeploymentRunTracker(nil)
	log := logger.New(logger.LevelCritical)
	controlPlane := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		applicationCtx: appCtx,
		appConfig:      &app.Config{},
		log:            log,
		tracker:        tracker,
		pollRunner: func(ctx context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider, _ string,
		) error {
			close(started)
			<-ctx.Done()

			finished <- ctx.Err()

			return ctx.Err()
		},
	})

	requestCtx, cancelRequest := context.WithCancel(t.Context())

	jobID, err := controlPlane.TriggerPoll(requestCtx, []poll.Config{{SourceUrl: validPollSourceURL}}, false, log.Logger)
	if err != nil {
		t.Fatal(err)
	}

	cancelRequest()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("async poll did not start")
	}

	select {
	case err := <-finished:
		t.Fatalf("request cancellation stopped async poll: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	run, ok := tracker.Get(jobID)
	if !ok || run.Status != deploymentRunStatusRunning {
		t.Fatalf("async run is not resolvable while running: %#v, %t", run, ok)
	}

	cancelApp()

	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("application cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("application cancellation did not stop async poll")
	}

	controlPlane.background.Wait()
	waitForDeploymentRunStatus(t, tracker, jobID, deploymentRunStatusFailed)
}

func TestControlPlaneRunsTriggerPollAsyncRejectsWorkDuringShutdown(t *testing.T) {
	tracker := newDeploymentRunTracker(nil)
	background := newBackgroundWork()
	background.CloseAndWait()

	log := logger.New(logger.LevelCritical)
	controlPlane := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig:  &app.Config{},
		background: background,
		log:        log,
		tracker:    tracker,
		pollRunner: func(context.Context, poll.Config, *app.Config, container.MountPoint,
			command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, secretprovider.SecretProvider, string,
		) error {
			t.Fatal("poll run started during shutdown")

			return nil
		},
	})

	jobID, err := controlPlane.TriggerPoll(t.Context(), []poll.Config{{SourceUrl: validPollSourceURL}}, false, log.Logger)
	if !errors.Is(err, ErrBackgroundWorkClosed) {
		t.Fatalf("error = %v, want %v", err, ErrBackgroundWorkClosed)
	}

	run, ok := tracker.Get(jobID)
	if !ok || run.Status != deploymentRunStatusFailed || run.Message != ErrBackgroundWorkClosed.Error() {
		t.Fatalf("tracked run = %#v, found = %t", run, ok)
	}
}

func TestControlPlaneRunsTriggerPollWaitUsesRequestContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	log := logger.New(logger.LevelCritical)
	controlPlane := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig: &app.Config{},
		log:       log,
		pollRunner: func(ctx context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider, _ string,
		) error {
			return ctx.Err()
		},
	})

	_, err := controlPlane.TriggerPoll(ctx, []poll.Config{{SourceUrl: validPollSourceURL}}, true, log.Logger)

	failed, ok := errors.AsType[*PollRunsFailedError](err)
	if !ok || failed.Failed != 1 || failed.Total != 1 {
		t.Fatalf("wait=true cancellation result = %v", err)
	}
}

func TestControlPlaneRunsTriggerPollWaitUsesApplicationLifecycle(t *testing.T) {
	appCtx, cancelApp := context.WithCancel(t.Context())
	background := newBackgroundWork()
	tracker := newDeploymentRunTracker(nil)
	started := make(chan struct{})
	log := logger.New(logger.LevelCritical)
	controlPlane := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		applicationCtx: appCtx,
		appConfig:      &app.Config{},
		background:     background,
		log:            log,
		tracker:        tracker,
		pollRunner: func(ctx context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider, _ string,
		) error {
			close(started)
			<-ctx.Done()

			return ctx.Err()
		},
	})

	result := make(chan struct {
		jobID string
		err   error
	}, 1)
	go func() {
		jobID, err := controlPlane.TriggerPoll(t.Context(), []poll.Config{{SourceUrl: validPollSourceURL}}, true, log.Logger)
		result <- struct {
			jobID string
			err   error
		}{jobID, err}
	}()

	<-started
	cancelApp()
	controlPlane.CloseAndWait()

	runResult := <-result
	if runResult.err == nil {
		t.Fatal("expected lifecycle cancellation error")
	}

	run, ok := tracker.Get(runResult.jobID)
	if !ok || run.Status != deploymentRunStatusFailed {
		t.Fatalf("tracked lifecycle cancellation = %#v, found = %t", run, ok)
	}
}

func TestControlPlaneRunsTriggerPollWaitRejectsWorkDuringShutdown(t *testing.T) {
	tracker := newDeploymentRunTracker(nil)
	background := newBackgroundWork()
	background.CloseAndWait()

	log := logger.New(logger.LevelCritical)
	controlPlane := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig:  &app.Config{},
		background: background,
		log:        log,
		tracker:    tracker,
	})

	jobID, err := controlPlane.TriggerPoll(t.Context(), []poll.Config{{SourceUrl: validPollSourceURL}}, true, log.Logger)
	if !errors.Is(err, ErrBackgroundWorkClosed) {
		t.Fatalf("error = %v, want %v", err, ErrBackgroundWorkClosed)
	}

	run, ok := tracker.Get(jobID)
	if !ok || run.Status != deploymentRunStatusFailed || run.Message != ErrBackgroundWorkClosed.Error() {
		t.Fatalf("tracked rejected sync run = %#v, found = %t", run, ok)
	}
}
