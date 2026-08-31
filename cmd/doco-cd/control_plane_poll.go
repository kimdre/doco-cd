package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/source/oci"
)

var (
	errNoPollConfiguration       = errors.New("no poll configuration provided in request body")
	errTooManyPollConfigurations = errors.New("too many poll configurations: maximum is 32")
	errPollRunPanicked           = errors.New("poll run panicked")
)

const (
	maxTriggerPollConfigs           = 32
	defaultConcurrentPollExecutions = 4
)

type pollConfigValidationError struct {
	Index int
	Err   error
}

func (e *pollConfigValidationError) Error() string {
	return fmt.Sprintf("invalid poll configuration at index %d: %v", e.Index, e.Err)
}

func (e *pollConfigValidationError) Unwrap() error {
	return e.Err
}

type pollRunsFailedError struct {
	Failed int
	Total  int
	Cause  error
}

func (e *pollRunsFailedError) Error() string {
	return fmt.Sprintf("%d/%d poll jobs failed", e.Failed, e.Total)
}

func (e *pollRunsFailedError) Unwrap() error {
	return e.Cause
}

type pollRunner func(ctx context.Context, pollConfig poll.Config, appConfig *app.Config, dataMountPoint container.MountPoint,
	dockerCli command.Cli, contexts *docker.ContextRegistry, logger *slog.Logger, metadata notification.Metadata, secretProvider *secretprovider.SecretProvider,
	triggerReason string,
) error

type controlPlanePoll struct {
	appConfig      *app.Config
	dataMountPoint container.MountPoint
	dockerCli      command.Cli
	contexts       *docker.ContextRegistry
	secretProvider *secretprovider.SecretProvider
	run            pollRunner
}

func newControlPlanePoll(
	appConfig *app.Config,
	dataMountPoint container.MountPoint,
	dockerCli command.Cli,
	contexts *docker.ContextRegistry,
	secretProvider *secretprovider.SecretProvider,
	run pollRunner,
) *controlPlanePoll {
	if appConfig == nil {
		panic("poll application config is required")
	}

	if run == nil {
		panic("poll runner is required")
	}

	return &controlPlanePoll{
		appConfig:      appConfig,
		dataMountPoint: dataMountPoint,
		dockerCli:      dockerCli,
		contexts:       contexts,
		secretProvider: secretProvider,
		run:            run,
	}
}

func (c *controlPlaneRuns) TriggerPoll(ctx context.Context, configs []poll.Config, wait bool, jobLog *slog.Logger) (string, error) {
	jobID := id.New()
	if len(configs) == 0 {
		return jobID, errNoPollConfiguration
	}

	if len(configs) > maxTriggerPollConfigs {
		return jobID, errTooManyPollConfigurations
	}

	jobLog = jobLog.With(slog.String("job_id", jobID))

	for i := range configs {
		configs[i].RunOnce = true
		configs[i].Interval = 0

		if err := configs[i].Validate(); err != nil {
			return jobID, &pollConfigValidationError{Index: i, Err: err}
		}
	}

	repository := "multiple"
	target := ""

	if len(configs) == 1 {
		repository = pollRepositoryName(configs[0])
		target = configs[0].CustomTarget
	}

	c.Accept(jobID, deploymentRunTriggerPoll, controlPlaneRunMetadata{
		Repository: repository,
		Target:     target,
	})

	mode := controlPlaneRunAsynchronous
	if wait {
		mode = controlPlaneRunSynchronous
	}

	err := c.Execute(ctx, jobID, controlPlaneRunExecution{
		mode:         mode,
		panicContext: "poll run",
		panicError:   errPollRunPanicked,
	}, func(runCtx context.Context) (controlPlaneRunResult, error) {
		workerCount := defaultConcurrentPollExecutions
		if c.poll.appConfig.MaxConcurrentDeployments > 0 {
			workerCount = int(min(c.poll.appConfig.MaxConcurrentDeployments, uint(len(configs))))
		}

		workerCount = min(workerCount, len(configs))

		jobs := make(chan poll.Config)
		errs := make(chan error, len(configs))

		var wg sync.WaitGroup

		for range workerCount {
			wg.Go(func() {
				for cfg := range jobs {
					metadata := notification.Metadata{
						Repository:               pollRepositoryName(cfg),
						Target:                   cfg.CustomTarget,
						Revision:                 notification.GetRevision(cfg.Reference, ""),
						JobID:                    jobID,
						DeploymentTargetObserver: c.DeploymentTargetObserver(jobID),
					}
					errs <- c.runProtected(jobLog, "poll run", errPollRunPanicked, func() error {
						return c.poll.run(
							runCtx,
							cfg,
							c.poll.appConfig,
							c.poll.dataMountPoint,
							c.poll.dockerCli,
							c.poll.contexts,
							jobLog,
							metadata,
							c.poll.secretProvider,
							pollTriggerDefault,
						)
					})
				}
			})
		}

		for _, cfg := range configs {
			jobs <- cfg
		}

		close(jobs)

		wg.Wait()
		close(errs)

		failedRuns := 0

		var lifecycleErr error

		for runErr := range errs {
			if runErr != nil {
				failedRuns++

				if lifecycleErr == nil && isLifecycleCancellation(runErr) {
					lifecycleErr = runErr
				}
			}
		}

		if failedRuns > 0 {
			err := &pollRunsFailedError{Failed: failedRuns, Total: len(configs), Cause: lifecycleErr}

			return failedControlPlaneRun(err.Error()), err
		}

		return succeededControlPlaneRun("poll jobs complete"), nil
	})

	return jobID, err
}

func (c *controlPlaneRuns) RunConfiguredPoll(
	ctx context.Context,
	pollConfig poll.Config,
	log *slog.Logger,
	triggerReason string,
) (string, error) {
	jobID := c.Accept("", deploymentRunTriggerPoll, controlPlaneRunMetadata{
		Repository: pollRepositoryName(pollConfig),
		Target:     pollConfig.CustomTarget,
		Revision:   notification.GetRevision(pollConfig.Reference, ""),
	})
	metadata := notification.Metadata{
		Repository:               pollRepositoryName(pollConfig),
		Target:                   pollConfig.CustomTarget,
		Revision:                 notification.GetRevision(pollConfig.Reference, ""),
		JobID:                    jobID,
		DeploymentTargetObserver: c.DeploymentTargetObserver(jobID),
	}

	err := c.Execute(ctx, jobID, controlPlaneRunExecution{
		mode:         controlPlaneRunSynchronous,
		panicContext: "poll run",
		panicError:   errPollRunPanicked,
	}, func(runCtx context.Context) (controlPlaneRunResult, error) {
		err := c.poll.run(
			runCtx,
			pollConfig,
			c.poll.appConfig,
			c.poll.dataMountPoint,
			c.poll.dockerCli,
			c.poll.contexts,
			log,
			metadata,
			c.poll.secretProvider,
			triggerReason,
		)
		if err != nil {
			return failedControlPlaneRun(err.Error()), err
		}

		return succeededControlPlaneRun("poll completed successfully"), nil
	})

	return jobID, err
}

func (c *controlPlaneRuns) runProtected(log *slog.Logger, panicContext string, panicError error, run func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logRecoveredPanic(log, panicContext, recovered)

			err = panicError
		}
	}()

	return run()
}

func pollRepositoryName(cfg poll.Config) string {
	sourceType := config.NormalizeSourceType(cfg.Source)
	if sourceType == config.SourceTypeOCI {
		return oci.RepositoryNameFromArtifact(cfg.SourceUrl)
	}

	return git.GetRepoName(cfg.SourceUrl)
}
