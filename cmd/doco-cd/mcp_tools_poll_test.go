package main

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
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

const validPollSourceURL = "https://github.com/kimdre/doco-cd_tests.git"

func TestRunPollConfigsAppliesDefaultsAndReportsIndexedValidationErrors(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		var got poll.Config

		h := &handlerData{
			appConfig: &app.Config{},
			log:       logger.New(logger.LevelCritical),
			runPoll: func(_ context.Context, cfg poll.Config, _ *app.Config, _ container.MountPoint,
				_ command.Cli, _ *slog.Logger, _ notification.Metadata, _ *secretprovider.SecretProvider,
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
				command.Cli, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider,
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
				command.Cli, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider,
			) error {
				return nil
			},
			wantStatus:  deploymentRunStatusSucceeded,
			wantMessage: "poll jobs complete",
		},
		{
			name: "failure",
			run: func(context.Context, poll.Config, *app.Config, container.MountPoint,
				command.Cli, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider,
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
				command.Cli, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider,
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
			_ command.Cli, _ *slog.Logger, _ notification.Metadata, _ *secretprovider.SecretProvider,
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

func TestRunPollConfigsWaitUsesRequestContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h := &handlerData{
		appConfig: &app.Config{},
		log:       logger.New(logger.LevelCritical),
		runPoll: func(ctx context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *slog.Logger, _ notification.Metadata, _ *secretprovider.SecretProvider,
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
