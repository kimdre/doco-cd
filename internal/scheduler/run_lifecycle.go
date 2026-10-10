package scheduler

import (
	"errors"
	"time"
)

var ErrScheduledRunPanicked = errors.New("scheduled job run panicked")

// RunExecution identifies an execution after its source job has been resolved.
type RunExecution struct {
	RunID     string
	JobName   string
	Context   string
	Stack     string
	Mode      string
	StartedAt time.Time
}

// RunReporter connects scheduler executions to the shared run history.
type RunReporter interface {
	ScheduledRunStarted(RunExecution)
	ScheduledRunFinished(string, error)
}

// startTrackedRun records the start of a scheduled run in the shared run history.
func (s *scheduler) startTrackedRun(job scheduledJob, runID string, startedAt time.Time) {
	if s.runReporter != nil {
		s.runReporter.ScheduledRunStarted(RunExecution{
			RunID:     runID,
			JobName:   job.name,
			Context:   job.context,
			Stack:     getJobStackName(job),
			Mode:      string(job.mode),
			StartedAt: startedAt,
		})
	}

	s.runtime.setLatestRun(job.key, runID, startedAt)
}

// finishTrackedRun records panics before the outer worker reports them.
func (s *scheduler) finishTrackedRun(runID string, runErr *error, recovered any) {
	if recovered != nil {
		*runErr = ErrScheduledRunPanicked
	}

	if s.runReporter != nil {
		s.runReporter.ScheduledRunFinished(runID, *runErr)
	}

	if recovered != nil {
		panic(recovered)
	}
}
