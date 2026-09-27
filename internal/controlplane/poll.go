package controlplane

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
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/source/oci"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

var (
	// ErrNoPollConfiguration indicates an empty poll-trigger request.
	ErrNoPollConfiguration = errors.New("no poll configuration provided in request body")
	// ErrTooManyPollConfigurations indicates that a request exceeds MaxTriggerPollConfigs.
	ErrTooManyPollConfigurations = errors.New("too many poll configurations: maximum is 32")
	// ErrPollRunPanicked is recorded when a poll callback panics.
	ErrPollRunPanicked = errors.New("poll run panicked")
)

const (
	// MaxTriggerPollConfigs limits the number of poll configurations accepted per request.
	MaxTriggerPollConfigs           = 32
	defaultConcurrentPollExecutions = 4
	defaultPollTrigger              = "poll"
)

// PollConfigValidationError identifies a malformed poll configuration by request index.
type PollConfigValidationError struct {
	Index int
	Err   error
}

// Error identifies the invalid poll configuration by request index.
func (e *PollConfigValidationError) Error() string {
	return fmt.Sprintf("invalid poll configuration at index %d: %v", e.Index, e.Err)
}

// Unwrap exposes the underlying poll configuration validation error.
func (e *PollConfigValidationError) Unwrap() error {
	return e.Err
}

// PollRunsFailedError summarizes failures from a batch of poll configurations.
type PollRunsFailedError struct {
	Failed int
	Total  int
	Cause  error
}

// Error reports the number of failed poll runs.
func (e *PollRunsFailedError) Error() string {
	return fmt.Sprintf("%d/%d poll jobs failed", e.Failed, e.Total)
}

// Unwrap exposes the first poll-run failure.
func (e *PollRunsFailedError) Unwrap() error {
	return e.Cause
}

// PollRunner executes one validated poll configuration with application dependencies.
type PollRunner func(ctx context.Context, pollConfig poll.Config, appConfig *app.Config, dataMountPoint container.MountPoint,
	dockerCli command.Cli, contexts *docker.ContextRegistry, logger *slog.Logger, metadata notification.Metadata, secretProvider secretprovider.SecretProvider,
	triggerReason string,
) error

// PollDependencies contains the services required to execute poll configurations.
type PollDependencies struct {
	AppConfig      *app.Config `validate:"required,nostructlevel"`
	DataMountPoint container.MountPoint
	DockerCLI      command.Cli
	Contexts       *docker.ContextRegistry
	Runner         PollRunner `validate:"required"`
}

type controlPlanePoll struct {
	appConfig      *app.Config
	dataMountPoint container.MountPoint
	dockerCli      command.Cli
	contexts       *docker.ContextRegistry
	secretProvider secretprovider.SecretProvider
	run            PollRunner
}

func newControlPlanePoll(
	appConfig *app.Config,
	dataMountPoint container.MountPoint,
	dockerCli command.Cli,
	contexts *docker.ContextRegistry,
	secretProvider secretprovider.SecretProvider,
	run PollRunner,
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

// TriggerPoll validates and executes a bounded batch of one-shot poll configurations.
// If sync windows deferred every configuration of a synchronous batch, it
// returns a *stages.SyncWindowBlockedError and the run is marked skipped.
func (c *Runs) TriggerPoll(ctx context.Context, configs []poll.Config, wait bool, jobLog *slog.Logger) (string, error) {
	jobID := id.New()
	if len(configs) == 0 {
		return jobID, ErrNoPollConfiguration
	}

	if len(configs) > MaxTriggerPollConfigs {
		return jobID, ErrTooManyPollConfigurations
	}

	jobLog = jobLog.With(slog.String("job_id", jobID))

	for i := range configs {
		configs[i].RunOnce = true
		configs[i].Interval = 0
		configs[i].Schedule = ""

		if err := configs[i].Validate(); err != nil {
			return jobID, &PollConfigValidationError{Index: i, Err: err}
		}
	}

	repository := "multiple"
	target := ""

	if len(configs) == 1 {
		repository = pollRepositoryName(configs[0])
		target = configs[0].CustomTarget
	}

	c.Accept(jobID, deploymentRunTriggerPoll, RunMetadata{
		Repository: repository,
		Target:     target,
	})

	mode := RunAsynchronous
	if wait {
		mode = RunSynchronous
	}

	err := c.Execute(ctx, jobID, RunExecution{
		Mode:         mode,
		PanicContext: "poll run",
		PanicError:   ErrPollRunPanicked,
	}, func(runCtx context.Context) (RunResult, error) {
		// Polls triggered through the API or MCP are manual deployments,
		// which sync windows with manual_sync let through.
		runCtx = WithDeploymentOrigin(runCtx, syncwindow.OriginManual)

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
					errs <- c.runProtected(jobLog, "poll run", ErrPollRunPanicked, func() error {
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
							defaultPollTrigger,
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

		var (
			lifecycleErr error
			deferred     []*stages.SyncWindowBlockedError
		)

		for runErr := range errs {
			if runErr == nil {
				continue
			}

			if blocked, ok := errors.AsType[*stages.SyncWindowBlockedError](runErr); ok {
				deferred = append(deferred, blocked)

				continue
			}

			failedRuns++

			if lifecycleErr == nil && IsLifecycleCancellation(runErr) {
				lifecycleErr = runErr
			}
		}

		if failedRuns > 0 {
			err := &PollRunsFailedError{Failed: failedRuns, Total: len(configs), Cause: lifecycleErr}

			return FailedRun(err.Error()), err
		}

		// The error tells synchronous callers that nothing was deployed; the
		// run itself is skipped, not failed.
		if len(deferred) == len(configs) {
			blocked := stages.MergeSyncWindowBlocked(deferred)

			return SkippedRun(blocked.Error()), blocked
		}

		return SucceededRun("poll jobs complete"), nil
	})

	return jobID, err
}

// RunConfiguredPoll executes one prevalidated scheduled poll under the shared lifecycle.
func (c *Runs) RunConfiguredPoll(
	ctx context.Context,
	pollConfig poll.Config,
	log *slog.Logger,
	triggerReason string,
) (string, error) {
	jobID := c.Accept("", deploymentRunTriggerPoll, RunMetadata{
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

	err := c.Execute(ctx, jobID, RunExecution{
		Mode:         RunSynchronous,
		PanicContext: "poll run",
		PanicError:   ErrPollRunPanicked,
	}, func(runCtx context.Context) (RunResult, error) {
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
		if errors.Is(err, stages.ErrSyncWindowBlocked) {
			return SkippedRun(err.Error()), nil
		}

		if err != nil {
			return FailedRun(err.Error()), err
		}

		return SucceededRun("poll completed successfully"), nil
	})

	return jobID, err
}

// runProtected converts a callback panic into the supplied stable error.
func (c *Runs) runProtected(log *slog.Logger, panicContext string, panicError error, run func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.LogRecoveredPanic(log, panicContext, recovered)

			err = panicError
		}
	}()

	return run()
}

// pollRepositoryName returns a stable repository identifier for Git and OCI poll sources.
func pollRepositoryName(cfg poll.Config) string {
	sourceType := config.NormalizeSourceType(cfg.Source)
	if sourceType == config.SourceTypeOCI {
		return oci.RepositoryNameFromArtifact(cfg.SourceUrl)
	}

	return git.GetRepoName(cfg.SourceUrl)
}
