package controlplane

import (
	"context"
	"errors"
	"log/slog"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

type RunMode uint8

const (
	RunSynchronous RunMode = iota
	RunSynchronousDetached
	RunAsynchronous
)

type RunMetadata struct {
	Repository string
	Target     string
	Revision   string
}

type RunResult struct {
	Status  RunStatus
	Message string
}

type RunExecution struct {
	Mode         RunMode
	PanicContext string
	PanicError   error
}

type RunFunc func(context.Context) (RunResult, error)

// Runs coordinates control-plane run admission and lifecycle while
// delegating storage and draining to their focused components.
type Runs struct {
	applicationCtx    context.Context
	cancelApplication context.CancelFunc
	background        *backgroundWork
	tracker           *deploymentRunTracker
	log               *slog.Logger
	scheduledJobs     *controlPlaneJobs
	poll              *controlPlanePoll
}

type Dependencies struct {
	MaxRunsPerTrigger map[RunTrigger]int
	ScheduledJobs     ScheduledJobOperations
	SecretProvider    *secretprovider.SecretProvider
	Poll              PollDependencies
}

func NewRuns(applicationCtx context.Context, log *slog.Logger, dependencies Dependencies) *Runs {
	return newRuns(
		applicationCtx,
		newBackgroundWork(),
		newDeploymentRunTracker(dependencies.MaxRunsPerTrigger),
		log,
		newControlPlaneJobs(dependencies.ScheduledJobs, dependencies.SecretProvider),
		newControlPlanePoll(
			dependencies.Poll.AppConfig,
			dependencies.Poll.DataMountPoint,
			dependencies.Poll.DockerCLI,
			dependencies.Poll.Contexts,
			dependencies.SecretProvider,
			dependencies.Poll.Runner,
		),
	)
}

func newRuns(
	applicationCtx context.Context,
	background *backgroundWork,
	tracker *deploymentRunTracker,
	log *slog.Logger,
	scheduledJobs *controlPlaneJobs,
	poll *controlPlanePoll,
) *Runs {
	if applicationCtx == nil {
		panic("control plane runs application context is required")
	}

	if background == nil {
		panic("control plane runs background work is required")
	}

	if tracker == nil {
		panic("control plane runs tracker is required")
	}

	if log == nil {
		panic("control plane runs logger is required")
	}

	if scheduledJobs == nil {
		panic("control plane scheduled jobs are required")
	}

	if poll == nil {
		panic("control plane poll operations are required")
	}

	runCtx, cancel := context.WithCancel(applicationCtx)

	return &Runs{
		applicationCtx:    runCtx,
		cancelApplication: cancel,
		background:        background,
		tracker:           tracker,
		log:               log,
		scheduledJobs:     scheduledJobs,
		poll:              poll,
	}
}

func (c *Runs) Accept(jobID string, trigger RunTrigger, metadata RunMetadata) string {
	if jobID == "" {
		jobID = id.New()
	}

	c.tracker.TrackAccepted(jobID, trigger)
	c.SetMetadata(jobID, metadata)

	return jobID
}

func (c *Runs) SetMetadata(jobID string, metadata RunMetadata) {
	c.tracker.SetMetadata(jobID, metadata.Repository, metadata.Target, metadata.Revision)
}

func (c *Runs) AddDeployment(jobID, stack, contextName string) {
	c.tracker.AddDeployment(jobID, stack, contextName)
}

func (c *Runs) DeploymentTargetObserver(jobID string) func(string, string) {
	return func(stack, contextName string) {
		c.AddDeployment(jobID, stack, contextName)
	}
}

func (c *Runs) MarkRunning(jobID string) {
	c.tracker.MarkRunning(jobID)
}

func (c *Runs) MarkFailed(jobID, message string) {
	c.tracker.MarkFailed(jobID, message)
}

func (c *Runs) MarkSkipped(jobID, message string) {
	c.tracker.MarkSkipped(jobID, message)
}

func (c *Runs) Get(jobID string) (Run, bool) {
	return c.tracker.Get(jobID)
}

func (c *Runs) List(limit int, trigger, status string) []Run {
	return c.tracker.List(limit, trigger, status)
}

func (c *Runs) Execute(
	requestCtx context.Context,
	jobID string,
	execution RunExecution,
	run RunFunc,
) error {
	runRegistered := func(runCtx context.Context) error {
		return c.executeRegistered(runCtx, jobID, execution, run)
	}

	if execution.Mode == RunAsynchronous {
		err := c.background.Go(func() {
			_ = runRegistered(c.applicationCtx)
		})
		if err != nil {
			c.MarkFailed(jobID, err.Error())
		}

		return err
	}

	release, err := c.background.Register()
	if err != nil {
		c.MarkFailed(jobID, err.Error())

		return err
	}
	defer release()

	if execution.Mode == RunSynchronousDetached {
		requestCtx = context.WithoutCancel(requestCtx)
	}

	runCtx, cancel := context.WithCancel(requestCtx)
	defer cancel()

	stopApplicationCancel := context.AfterFunc(c.applicationCtx, cancel) //nolint:contextcheck // A synchronous run is intentionally cancelled by either its request or the application lifecycle.
	defer stopApplicationCancel()

	return runRegistered(runCtx)
}

func (c *Runs) executeRegistered(
	ctx context.Context,
	jobID string,
	execution RunExecution,
	run RunFunc,
) (err error) {
	c.MarkRunning(jobID)

	defer func() {
		if recovered := recover(); recovered != nil {
			logger.LogRecoveredPanic(c.log.With(slog.String("job_id", jobID)), execution.PanicContext, recovered)
			c.MarkFailed(jobID, execution.PanicError.Error())
			err = execution.PanicError
		}
	}()

	result, err := run(ctx)
	c.finish(jobID, result, err)

	return err
}

func (c *Runs) finish(jobID string, result RunResult, err error) {
	if result.Status == "" {
		if err != nil {
			c.MarkFailed(jobID, err.Error())

			return
		}

		result.Status = deploymentRunStatusSucceeded
	}

	switch result.Status {
	case deploymentRunStatusSucceeded:
		c.tracker.MarkSucceeded(jobID, result.Message)
	case deploymentRunStatusFailed:
		message := result.Message
		if message == "" && err != nil {
			message = err.Error()
		}

		c.MarkFailed(jobID, message)
	case deploymentRunStatusSkipped:
		c.MarkSkipped(jobID, result.Message)
	default:
		panic("control plane run returned a non-terminal status")
	}
}

func (c *Runs) CloseAndWait() {
	c.background.Close()
	c.cancelApplication()
	c.background.Wait()
}

func SucceededRun(message string) RunResult {
	return RunResult{Status: deploymentRunStatusSucceeded, Message: message}
}

func FailedRun(message string) RunResult {
	return RunResult{Status: deploymentRunStatusFailed, Message: message}
}

func SkippedRun(message string) RunResult {
	return RunResult{Status: deploymentRunStatusSkipped, Message: message}
}

func IsLifecycleCancellation(err error) bool {
	return errors.Is(err, ErrBackgroundWorkClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
