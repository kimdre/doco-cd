package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/kimdre/doco-cd/internal/common/id"
)

type controlPlaneRunMode uint8

const (
	controlPlaneRunSynchronous controlPlaneRunMode = iota
	controlPlaneRunSynchronousDetached
	controlPlaneRunAsynchronous
)

type controlPlaneRunMetadata struct {
	Repository string
	Target     string
	Revision   string
}

type controlPlaneRunResult struct {
	status  deploymentRunStatus
	message string
}

type controlPlaneRunExecution struct {
	mode         controlPlaneRunMode
	panicContext string
	panicError   error
}

type controlPlaneRunFunc func(context.Context) (controlPlaneRunResult, error)

// controlPlaneRuns coordinates control-plane run admission and lifecycle while
// delegating storage and draining to their focused components.
type controlPlaneRuns struct {
	applicationCtx    context.Context
	cancelApplication context.CancelFunc
	background        *backgroundWork
	tracker           *deploymentRunTracker
	log               *slog.Logger
	scheduledJobs     *controlPlaneJobs
	poll              *controlPlanePoll
}

func newControlPlaneRuns(
	applicationCtx context.Context,
	background *backgroundWork,
	tracker *deploymentRunTracker,
	log *slog.Logger,
	scheduledJobs *controlPlaneJobs,
	poll *controlPlanePoll,
) *controlPlaneRuns {
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

	return &controlPlaneRuns{
		applicationCtx:    runCtx,
		cancelApplication: cancel,
		background:        background,
		tracker:           tracker,
		log:               log,
		scheduledJobs:     scheduledJobs,
		poll:              poll,
	}
}

func (c *controlPlaneRuns) Accept(jobID string, trigger deploymentRunTrigger, metadata controlPlaneRunMetadata) string {
	if jobID == "" {
		jobID = id.New()
	}

	c.tracker.TrackAccepted(jobID, trigger)
	c.SetMetadata(jobID, metadata)

	return jobID
}

func (c *controlPlaneRuns) SetMetadata(jobID string, metadata controlPlaneRunMetadata) {
	c.tracker.SetMetadata(jobID, metadata.Repository, metadata.Target, metadata.Revision)
}

func (c *controlPlaneRuns) AddDeployment(jobID, stack, contextName string) {
	c.tracker.AddDeployment(jobID, stack, contextName)
}

func (c *controlPlaneRuns) DeploymentTargetObserver(jobID string) func(string, string) {
	return func(stack, contextName string) {
		c.AddDeployment(jobID, stack, contextName)
	}
}

func (c *controlPlaneRuns) MarkRunning(jobID string) {
	c.tracker.MarkRunning(jobID)
}

func (c *controlPlaneRuns) MarkFailed(jobID, message string) {
	c.tracker.MarkFailed(jobID, message)
}

func (c *controlPlaneRuns) MarkSkipped(jobID, message string) {
	c.tracker.MarkSkipped(jobID, message)
}

func (c *controlPlaneRuns) Get(jobID string) (deploymentRun, bool) {
	return c.tracker.Get(jobID)
}

func (c *controlPlaneRuns) List(limit int, trigger, status string) []deploymentRun {
	return c.tracker.List(limit, trigger, status)
}

func (c *controlPlaneRuns) Execute(
	requestCtx context.Context,
	jobID string,
	execution controlPlaneRunExecution,
	run controlPlaneRunFunc,
) error {
	runRegistered := func(runCtx context.Context) error {
		return c.executeRegistered(runCtx, jobID, execution, run)
	}

	if execution.mode == controlPlaneRunAsynchronous {
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

	if execution.mode == controlPlaneRunSynchronousDetached {
		requestCtx = context.WithoutCancel(requestCtx)
	}

	runCtx, cancel := context.WithCancel(requestCtx)
	defer cancel()

	stopApplicationCancel := context.AfterFunc(c.applicationCtx, cancel) //nolint:contextcheck // A synchronous run is intentionally cancelled by either its request or the application lifecycle.
	defer stopApplicationCancel()

	return runRegistered(runCtx)
}

func (c *controlPlaneRuns) executeRegistered(
	ctx context.Context,
	jobID string,
	execution controlPlaneRunExecution,
	run controlPlaneRunFunc,
) (err error) {
	c.MarkRunning(jobID)

	defer func() {
		if recovered := recover(); recovered != nil {
			logRecoveredPanic(c.log.With(slog.String("job_id", jobID)), execution.panicContext, recovered)
			c.MarkFailed(jobID, execution.panicError.Error())
			err = execution.panicError
		}
	}()

	result, err := run(ctx)
	c.finish(jobID, result, err)

	return err
}

func (c *controlPlaneRuns) finish(jobID string, result controlPlaneRunResult, err error) {
	if result.status == "" {
		if err != nil {
			c.MarkFailed(jobID, err.Error())

			return
		}

		result.status = deploymentRunStatusSucceeded
	}

	switch result.status {
	case deploymentRunStatusSucceeded:
		c.tracker.MarkSucceeded(jobID, result.message)
	case deploymentRunStatusFailed:
		message := result.message
		if message == "" && err != nil {
			message = err.Error()
		}

		c.MarkFailed(jobID, message)
	case deploymentRunStatusSkipped:
		c.MarkSkipped(jobID, result.message)
	default:
		panic("control plane run returned a non-terminal status")
	}
}

func (c *controlPlaneRuns) CloseAndWait() {
	c.background.Close()
	c.cancelApplication()
	c.background.Wait()
}

func succeededControlPlaneRun(message string) controlPlaneRunResult {
	return controlPlaneRunResult{status: deploymentRunStatusSucceeded, message: message}
}

func failedControlPlaneRun(message string) controlPlaneRunResult {
	return controlPlaneRunResult{status: deploymentRunStatusFailed, message: message}
}

func skippedControlPlaneRun(message string) controlPlaneRunResult {
	return controlPlaneRunResult{status: deploymentRunStatusSkipped, message: message}
}

func isLifecycleCancellation(err error) bool {
	return errors.Is(err, errBackgroundWorkClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
