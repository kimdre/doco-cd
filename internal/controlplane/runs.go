package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

// RunMode controls whether a run blocks its caller and whether request cancellation propagates.
type RunMode uint8

const (
	// RunSynchronous runs inline and stops when either the request or application is cancelled.
	RunSynchronous RunMode = iota
	// RunSynchronousDetached runs inline but ignores request cancellation.
	RunSynchronousDetached
	// RunAsynchronous runs in the background under the application lifecycle.
	RunAsynchronous
)

// RunMetadata describes the source and deployment target recorded for a run.
type RunMetadata struct {
	Repository string
	Target     string
	Revision   string
}

// RunResult describes the terminal status and optional message produced by a run.
type RunResult struct {
	Status  RunStatus
	Message string
}

// RunExecution configures lifecycle behavior and panic reporting for one run.
type RunExecution struct {
	Mode         RunMode
	PanicContext string
	PanicError   error
}

// RunFunc performs the work associated with an accepted control-plane run.
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

// Dependencies contains the operations and limits used by the run coordinator.
type Dependencies struct {
	MaxRunsPerTrigger map[RunTrigger]int     `validate:"omitempty,dive,keys,oneof=webhook poll scheduled_job,endkeys,min=1"`
	ScheduledJobs     ScheduledJobOperations `validate:"required,nostructlevel"`
	SecretProvider    *secretprovider.SecretProvider
	Poll              PollDependencies
}

// NewRuns validates dependencies and constructs a control-plane run coordinator.
func NewRuns(applicationCtx context.Context, log *slog.Logger, dependencies Dependencies) *Runs {
	if err := validation.Validate(dependencies); err != nil {
		panic(fmt.Errorf("validate control-plane dependencies: %w", err))
	}

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

// Accept records a run in accepted state, generating a job ID when needed.
func (c *Runs) Accept(jobID string, trigger RunTrigger, metadata RunMetadata) string {
	if jobID == "" {
		jobID = id.New()
	}

	c.tracker.TrackAccepted(jobID, trigger)
	c.SetMetadata(jobID, metadata)

	return jobID
}

// SetMetadata updates source and target metadata for a tracked run.
func (c *Runs) SetMetadata(jobID string, metadata RunMetadata) {
	c.tracker.SetMetadata(jobID, metadata.Repository, metadata.Target, metadata.Revision)
}

// AddDeployment records a stack and Docker context observed during a run.
func (c *Runs) AddDeployment(jobID, stack, contextName string) {
	c.tracker.AddDeployment(jobID, stack, contextName)
}

// DeploymentTargetObserver returns a callback that records deployments for jobID.
func (c *Runs) DeploymentTargetObserver(jobID string) func(string, string) {
	return func(stack, contextName string) {
		c.AddDeployment(jobID, stack, contextName)
	}
}

// MarkRunning transitions a tracked run to running.
func (c *Runs) MarkRunning(jobID string) {
	c.tracker.MarkRunning(jobID)
}

// MarkFailed transitions a tracked run to failed with a message.
func (c *Runs) MarkFailed(jobID, message string) {
	c.tracker.MarkFailed(jobID, message)
}

// MarkSkipped transitions a tracked run to skipped with a message.
func (c *Runs) MarkSkipped(jobID, message string) {
	c.tracker.MarkSkipped(jobID, message)
}

// Get returns a defensive copy of a run by job ID.
func (c *Runs) Get(jobID string) (Run, bool) {
	return c.tracker.Get(jobID)
}

// List returns filtered runs in reverse creation order.
func (c *Runs) List(limit int, trigger, status string) []Run {
	return c.tracker.List(limit, trigger, status)
}

// Execute runs accepted work in the configured lifecycle mode.
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

// executeRegistered transitions an accepted run through execution, recovery, and finalization.
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

// finish converts a run result and error into exactly one terminal tracker state.
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

// CloseAndWait rejects new work, cancels active runs, and waits for them to finish.
func (c *Runs) CloseAndWait() {
	c.background.Close()
	c.cancelApplication()
	c.background.Wait()
}

// SucceededRun creates a successful terminal result.
func SucceededRun(message string) RunResult {
	return RunResult{Status: deploymentRunStatusSucceeded, Message: message}
}

// FailedRun creates a failed terminal result.
func FailedRun(message string) RunResult {
	return RunResult{Status: deploymentRunStatusFailed, Message: message}
}

// SkippedRun creates a skipped terminal result.
func SkippedRun(message string) RunResult {
	return RunResult{Status: deploymentRunStatusSkipped, Message: message}
}

// IsLifecycleCancellation reports whether err represents context cancellation or timeout.
func IsLifecycleCancellation(err error) bool {
	return errors.Is(err, ErrBackgroundWorkClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
