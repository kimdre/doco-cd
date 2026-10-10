package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

// ScheduledJobOperations is the scheduler surface required by the control plane.
type ScheduledJobOperations interface {
	ListJobs(context.Context, string, string) ([]scheduler.JobInfo, error)
	TriggerNow(context.Context, string, string, string, string, secretprovider.SecretProvider) (string, error)
}

// ScheduledJobInfo includes the latest tracked execution of a scheduled job.
type ScheduledJobInfo struct {
	scheduler.JobInfo
	LastRun *Run `json:"last_run"`
}

var _ scheduler.RunReporter = (*Runs)(nil)

// controlPlaneJobs binds scheduler operations to the optional secret provider.
type controlPlaneJobs struct {
	operations     ScheduledJobOperations
	secretProvider secretprovider.SecretProvider
}

func newControlPlaneJobs(operations ScheduledJobOperations, secretProvider secretprovider.SecretProvider) *controlPlaneJobs {
	if operations == nil {
		panic("scheduled job operations are required")
	}

	return &controlPlaneJobs{
		operations:     operations,
		secretProvider: secretProvider,
	}
}

// ErrScheduledJobRunPanicked is recorded when a scheduled-job callback panics.
var ErrScheduledJobRunPanicked = errors.New("scheduled job run panicked")

// ListScheduledJobs returns scheduled jobs for an optional Docker context and stack.
func (c *Runs) ListScheduledJobs(ctx context.Context, contextName, stackName string) ([]ScheduledJobInfo, error) {
	jobs, err := c.scheduledJobs.operations.ListJobs(ctx, contextName, stackName)
	if jobs == nil {
		return nil, err
	}

	c.tracker.cleanup(time.Now().UTC())

	result := make([]ScheduledJobInfo, len(jobs))
	for i, job := range jobs {
		result[i].JobInfo = job
		result[i].LastRunAt = localTimestamp(job.LastRunAt)
		result[i].NextRunAt = localTimestamp(job.NextRunAt)

		result[i].LabelNextRunAt = localTimestamp(job.LabelNextRunAt)
		if job.LatestRunID != "" {
			if run, ok := c.tracker.Get(job.LatestRunID); ok {
				result[i].LastRun = &run
			}
		}
	}

	return result, err
}

// ScheduledRunStarted records a resolved scheduler execution without resetting an admitted run.
func (c *Runs) ScheduledRunStarted(execution scheduler.RunExecution) {
	c.tracker.TrackScheduledRunStarted(execution.RunID, execution.JobName, execution.Context, execution.Stack, execution.StartedAt)
}

// ScheduledRunFinished finalizes scheduler-owned executions at most once.
func (c *Runs) ScheduledRunFinished(runID string, err error) {
	c.tracker.MarkScheduledRunFinished(runID, err)
}

// TriggerScheduledJob accepts and executes one scheduled job under the shared run lifecycle.
func (c *Runs) TriggerScheduledJob(
	ctx context.Context,
	jobID string,
	contextName string,
	jobName string,
	stackName string,
	wait bool,
) (string, error) {
	jobID = c.Accept(jobID, deploymentRunTriggerScheduledJob, RunMetadata{
		Repository: "scheduled:" + jobName,
		Target:     stackName,
	})
	c.AddDeployment(jobID, stackName, contextName)

	jobLog := c.log.With(slog.String("job_id", jobID), slog.String("context", contextName))

	mode := RunAsynchronous
	if wait {
		mode = RunSynchronous
	}

	err := c.Execute(ctx, jobID, RunExecution{
		Mode:         mode,
		PanicContext: "scheduled job run",
		PanicError:   ErrScheduledJobRunPanicked,
	}, func(runCtx context.Context) (RunResult, error) {
		jobLog.Info("scheduled job run triggered", slog.String("job", jobName), slog.String("stack", stackName))

		scheduledRunID, err := c.scheduledJobs.operations.TriggerNow(
			runCtx,
			jobID,
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

			return FailedRun(err.Error()), err
		}

		return SucceededRun("scheduled job trigger completed"), nil
	})

	return jobID, err
}
