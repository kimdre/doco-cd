package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

type scheduledJobOperations interface {
	ListJobs(context.Context, string, string) ([]scheduler.JobInfo, error)
	TriggerNow(context.Context, string, string, string, *secretprovider.SecretProvider) (string, error)
}

type controlPlaneJobs struct {
	operations     scheduledJobOperations
	secretProvider *secretprovider.SecretProvider
}

func newControlPlaneJobs(operations scheduledJobOperations, secretProvider *secretprovider.SecretProvider) *controlPlaneJobs {
	if operations == nil {
		panic("scheduled job operations are required")
	}

	return &controlPlaneJobs{
		operations:     operations,
		secretProvider: secretProvider,
	}
}

var errScheduledJobRunPanicked = errors.New("scheduled job run panicked")

func (c *controlPlaneRuns) ListScheduledJobs(ctx context.Context, contextName, stackName string) ([]scheduler.JobInfo, error) {
	return c.scheduledJobs.operations.ListJobs(ctx, contextName, stackName)
}

func (c *controlPlaneRuns) TriggerScheduledJob(
	ctx context.Context,
	jobID string,
	contextName string,
	jobName string,
	stackName string,
	wait bool,
) (string, error) {
	jobID = c.Accept(jobID, deploymentRunTriggerScheduledJob, controlPlaneRunMetadata{
		Repository: "scheduled:" + jobName,
		Target:     stackName,
	})
	c.AddDeployment(jobID, stackName, contextName)

	jobLog := c.log.With(slog.String("job_id", jobID), slog.String("context", contextName))

	mode := controlPlaneRunAsynchronous
	if wait {
		mode = controlPlaneRunSynchronous
	}

	err := c.Execute(ctx, jobID, controlPlaneRunExecution{
		mode:         mode,
		panicContext: "scheduled job run",
		panicError:   errScheduledJobRunPanicked,
	}, func(runCtx context.Context) (controlPlaneRunResult, error) {
		jobLog.Info("scheduled job run triggered", slog.String("job", jobName), slog.String("stack", stackName))

		scheduledRunID, err := c.scheduledJobs.operations.TriggerNow(
			runCtx,
			contextName,
			jobName,
			stackName,
			c.scheduledJobs.secretProvider,
		)

		runLog := jobLog
		if scheduledRunID != "" {
			runLog = runLog.With(slog.String("scheduled_run_id", scheduledRunID))
		}

		if err != nil {
			runLog.With(logger.ErrAttr(err)).Error("failed to trigger scheduled job run", slog.String("job", jobName), slog.String("stack", stackName))

			return failedControlPlaneRun(err.Error()), err
		}

		return succeededControlPlaneRun("scheduled job trigger completed"), nil
	})

	return jobID, err
}
