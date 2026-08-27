package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
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
)

const validPollSourceURL = "https://github.com/kimdre/doco-cd_tests.git"

func TestRunPollConfigsAppliesDefaultsAndReportsIndexedValidationErrors(t *testing.T) {
	t.Run("empty configs", func(t *testing.T) {
		tracker := newDeploymentRunTracker(nil)
		runs := 0
		h := &handlerData{
			appConfig:  &app.Config{},
			log:        logger.New(logger.LevelCritical),
			runTracker: tracker,
			runPoll: func(context.Context, poll.Config, *app.Config, container.MountPoint,
				command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider, string,
			) error {
				runs++

				return nil
			},
		}

		jobID, err := h.runPollConfigs(t.Context(), nil, true, h.log.Logger)
		if !errors.Is(err, errNoPollConfiguration) {
			t.Fatalf("empty configs error = %v", err)
		}

		if jobID == "" {
			t.Fatal("expected generated job ID")
		}

		if _, ok := tracker.Get(jobID); ok {
			t.Fatal("empty configs must not be tracked")
		}

		if runs != 0 {
			t.Fatalf("empty configs started %d poll runs", runs)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		var got poll.Config

		h := &handlerData{
			appConfig: &app.Config{},
			log:       logger.New(logger.LevelCritical),
			runPoll: func(_ context.Context, cfg poll.Config, _ *app.Config, _ container.MountPoint,
				_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ *secretprovider.SecretProvider, _ string,
			) error {
				got = cfg

				return nil
			},
		}

		jobID, err := h.runPollConfigs(t.Context(), []poll.Config{{SourceUrl: validPollSourceURL, Interval: time.Hour}}, true, h.log.Logger)
		if err != nil {
			t.Fatal(err)
		}

		if jobID == "" {
			t.Fatal("expected generated job ID")
		}

		if !got.RunOnce || got.Interval != 0 {
			t.Fatalf("poll defaults = run_once %t, interval %s", got.RunOnce, got.Interval)
		}
	})

	t.Run("indexed validation", func(t *testing.T) {
		tracker := newDeploymentRunTracker(nil)
		runs := 0
		h := &handlerData{
			appConfig:  &app.Config{},
			log:        logger.New(logger.LevelCritical),
			runTracker: tracker,
			runPoll: func(context.Context, poll.Config, *app.Config, container.MountPoint,
				command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider, string,
			) error {
				runs++

				return nil
			},
		}

		jobID, err := h.runPollConfigs(t.Context(), []poll.Config{{SourceUrl: validPollSourceURL}, {}}, true, h.log.Logger)
		if err == nil || err.Error() != "invalid poll configuration at index 1: key not found: url" {
			t.Fatalf("unexpected validation error: %v", err)
		}

		if jobID == "" {
			t.Fatal("expected job ID for validation response")
		}

		if runs != 0 {
			t.Fatalf("validation failure started %d poll runs", runs)
		}

		if _, ok := tracker.Get(jobID); ok {
			t.Fatal("validation failure must not be tracked")
		}
	})
}

func TestRunPollConfigsRejectsTooManyConfigs(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(nil)
	h := &handlerData{
		appConfig:  &app.Config{},
		log:        logger.New(logger.LevelCritical),
		runTracker: tracker,
	}

	configs := make([]poll.Config, maxTriggerPollConfigs+1)
	for i := range configs {
		configs[i].SourceUrl = validPollSourceURL
	}

	jobID, err := h.runPollConfigs(t.Context(), configs, true, h.log.Logger)
	if !errors.Is(err, errTooManyPollConfigurations) {
		t.Fatalf("error = %v, want %v", err, errTooManyPollConfigurations)
	}

	if _, ok := tracker.Get(jobID); ok {
		t.Fatal("rejected poll batch must not be tracked")
	}
}

func TestRunPollConfigsBoundsConcurrentRunners(t *testing.T) {
	t.Parallel()

	const maxConcurrent = 2

	var (
		running atomic.Int32
		peak    atomic.Int32
	)

	release := make(chan struct{})
	started := make(chan struct{}, maxTriggerPollConfigs)
	h := &handlerData{
		appConfig: &app.Config{MaxConcurrentDeployments: maxConcurrent},
		log:       logger.New(logger.LevelCritical),
		runPoll: func(context.Context, poll.Config, *app.Config, container.MountPoint,
			command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider, string,
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
	}

	configs := make([]poll.Config, 5)
	for i := range configs {
		configs[i].SourceUrl = validPollSourceURL
	}

	done := make(chan error, 1)

	go func() {
		_, err := h.runPollConfigs(t.Context(), configs, true, h.log.Logger)
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

func TestRunPollConfigsTracksSuccessFailureAndPanic(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		run         pollRunner
		wantStatus  deploymentRunStatus
		wantMessage string
		wantError   bool
	}{
		{
			name: "success",
			run: func(context.Context, poll.Config, *app.Config, container.MountPoint,
				command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider, string,
			) error {
				return nil
			},
			wantStatus:  deploymentRunStatusSucceeded,
			wantMessage: "poll jobs complete",
		},
		{
			name: "failure",
			run: func(context.Context, poll.Config, *app.Config, container.MountPoint,
				command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider, string,
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
				command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider, string,
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
			h := &handlerData{
				appConfig:  &app.Config{},
				log:        logger.New(logger.LevelCritical),
				runTracker: tracker,
				runPoll:    testCase.run,
			}

			jobID, err := h.runPollConfigs(t.Context(), []poll.Config{{SourceUrl: validPollSourceURL}}, true, h.log.Logger)
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

			if run.Repository != "doco-cd_tests" {
				t.Fatalf("repository metadata = %q", run.Repository)
			}
		})
	}
}

func TestRunPollConfigsAsyncUsesApplicationLifecycle(t *testing.T) {
	appCtx, cancelApp := context.WithCancel(context.Background())
	backgroundWG := &sync.WaitGroup{}
	started := make(chan struct{})
	finished := make(chan error, 1)
	tracker := newDeploymentRunTracker(nil)
	h := &handlerData{
		appConfig:     &app.Config{},
		backgroundCtx: appCtx,
		backgroundWG:  backgroundWG,
		log:           logger.New(logger.LevelCritical),
		runTracker:    tracker,
		runPoll: func(ctx context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ *secretprovider.SecretProvider, _ string,
		) error {
			close(started)
			<-ctx.Done()

			finished <- ctx.Err()

			return ctx.Err()
		},
	}

	requestCtx, cancelRequest := context.WithCancel(context.Background())

	jobID, err := h.runPollConfigs(requestCtx, []poll.Config{{SourceUrl: validPollSourceURL}}, false, h.log.Logger)
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

	backgroundWG.Wait()
	waitForDeploymentRunStatus(t, tracker, jobID, deploymentRunStatusFailed)
}

func TestRunPollConfigsAsyncRejectsWorkDuringShutdown(t *testing.T) {
	tracker := newDeploymentRunTracker(nil)
	background := newBackgroundWork()
	background.CloseAndWait()

	h := &handlerData{
		appConfig:      &app.Config{},
		backgroundCtx:  t.Context(),
		backgroundWork: background,
		log:            logger.New(logger.LevelCritical),
		runTracker:     tracker,
		runPoll: func(context.Context, poll.Config, *app.Config, container.MountPoint,
			command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider, string,
		) error {
			t.Fatal("poll run started during shutdown")

			return nil
		},
	}

	jobID, err := h.runPollConfigs(t.Context(), []poll.Config{{SourceUrl: validPollSourceURL}}, false, nil)
	if !errors.Is(err, errBackgroundWorkClosed) {
		t.Fatalf("error = %v, want %v", err, errBackgroundWorkClosed)
	}

	run, ok := tracker.Get(jobID)
	if !ok || run.Status != deploymentRunStatusFailed || run.Message != errBackgroundWorkClosed.Error() {
		t.Fatalf("tracked run = %#v, found = %t", run, ok)
	}
}

func TestRunPollConfigsWaitUsesRequestContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h := &handlerData{
		appConfig: &app.Config{},
		log:       logger.New(logger.LevelCritical),
		runPoll: func(ctx context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ *secretprovider.SecretProvider, _ string,
		) error {
			return ctx.Err()
		},
	}

	_, err := h.runPollConfigs(ctx, []poll.Config{{SourceUrl: validPollSourceURL}}, true, h.log.Logger)

	var failed *pollRunsFailedError
	if !errors.As(err, &failed) || failed.Failed != 1 || failed.Total != 1 {
		t.Fatalf("wait=true cancellation result = %v", err)
	}
}

func TestRunPollConfigsWaitUsesApplicationLifecycle(t *testing.T) {
	appCtx, cancelApp := context.WithCancel(context.Background())
	background := newBackgroundWork()
	tracker := newDeploymentRunTracker(nil)
	started := make(chan struct{})
	h := &handlerData{
		appConfig:      &app.Config{},
		backgroundCtx:  appCtx,
		backgroundWork: background,
		log:            logger.New(logger.LevelCritical),
		runTracker:     tracker,
		runPoll: func(ctx context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ *secretprovider.SecretProvider, _ string,
		) error {
			close(started)
			<-ctx.Done()

			return ctx.Err()
		},
	}

	result := make(chan struct {
		jobID string
		err   error
	}, 1)
	go func() {
		jobID, err := h.runPollConfigs(context.Background(), []poll.Config{{SourceUrl: validPollSourceURL}}, true, h.log.Logger)
		result <- struct {
			jobID string
			err   error
		}{jobID, err}
	}()

	<-started
	cancelApp()
	background.CloseAndWait()

	runResult := <-result
	if runResult.err == nil {
		t.Fatal("expected lifecycle cancellation error")
	}

	run, ok := tracker.Get(runResult.jobID)
	if !ok || run.Status != deploymentRunStatusFailed {
		t.Fatalf("tracked lifecycle cancellation = %#v, found = %t", run, ok)
	}
}

func TestRunPollConfigsWaitRejectsWorkDuringShutdown(t *testing.T) {
	tracker := newDeploymentRunTracker(nil)
	background := newBackgroundWork()
	background.CloseAndWait()
	h := &handlerData{
		appConfig:      &app.Config{},
		backgroundCtx:  t.Context(),
		backgroundWork: background,
		log:            logger.New(logger.LevelCritical),
		runTracker:     tracker,
	}

	jobID, err := h.runPollConfigs(t.Context(), []poll.Config{{SourceUrl: validPollSourceURL}}, true, h.log.Logger)
	if !errors.Is(err, errBackgroundWorkClosed) {
		t.Fatalf("error = %v, want %v", err, errBackgroundWorkClosed)
	}

	run, ok := tracker.Get(jobID)
	if !ok || run.Status != deploymentRunStatusFailed || run.Message != errBackgroundWorkClosed.Error() {
		t.Fatalf("tracked rejected sync run = %#v, found = %t", run, ok)
	}
}
