package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/prometheus"
)

func (s *scheduler) newExecutionRecord(runID string, job scheduledJob, cfg docker.JobScheduleConfig) executionRecord {
	return executionRecord{
		Version:      executionRecordVersion,
		RunID:        runID,
		Context:      s.contextName,
		Mode:         s.mode,
		Job:          executionJobFromScheduled(job),
		Finalization: executionFinalizationConfigFromSchedule(cfg),
	}
}

// completeExecutionRecord records delivery before removing the retained
// workload artifact. If this process dies between delivery and persistence,
// recovery can send the notification again, which is intentionally
// at-least-once rather than silently losing a terminal result.
func (s *scheduler) completeExecutionRecord(ctx context.Context, record *executionRecord) {
	if record == nil {
		return
	}

	record.Reported = true

	if err := s.executions.update(*record); err != nil {
		s.log.Error("failed to persist scheduled execution reporting state", slog.String("run_id", record.RunID), logger.ErrAttr(err))
		return
	}

	if err := s.cleanupExecutionArtifact(ctx, record); err != nil {
		s.log.Error("failed to clean up scheduled execution artifact", slog.String("run_id", record.RunID), logger.ErrAttr(err))
		return
	}

	if len(record.Finalization.StopServices) > 0 && !record.Restored {
		return
	}

	if err := s.executions.remove(record.RunID); err != nil {
		s.log.Error("failed to remove completed scheduled execution record", slog.String("run_id", record.RunID), logger.ErrAttr(err))
	}
}

func (s *scheduler) cleanupExecutionArtifact(ctx context.Context, record *executionRecord) error {
	switch record.Mode {
	case scheduledJobModeContainer:
		resourceID, err := docker.FindOneOffContainer(ctx, s.dockerCli.Client(), record.RunID)
		if err != nil {
			return err
		}

		if resourceID == "" {
			return nil
		}

		return docker.RemoveOneOffContainer(ctx, s.dockerCli.Client(), resourceID)
	case scheduledJobModeSwarm:
		resourceID, err := docker.FindSwarmOneOffService(ctx, s.dockerCli, record.RunID)
		if err != nil {
			return err
		}

		if resourceID == "" {
			return nil
		}

		return docker.RemoveSwarmOneOffService(ctx, s.dockerCli, resourceID, s.log)
	default:
		return fmt.Errorf("unsupported scheduled execution mode %q", record.Mode)
	}
}

// recoverExecutions finds in-flight jobs by RunID label and continues monitoring
// them until completion. Runs on startup after forced termination or crash.
func (s *scheduler) recoverExecutions(ctx context.Context) {
	records, err := s.executions.list(s.contextName, s.mode)
	if err != nil {
		s.log.Error("failed to load scheduled execution recovery records", logger.ErrAttr(err))
		return
	}

	for _, record := range records {
		s.recoverExecutionCandidate(ctx, record)
	}
}

// recoverExecutionCandidate attempts to claim ownership of a scheduled execution
// record and continue monitoring it until completion. If the record is no longer
// valid or the run has already completed, it is removed from the database.
func (s *scheduler) recoverExecutionCandidate(ctx context.Context, candidate executionRecord) {
	if !s.claimRecovery(candidate.RunID) {
		return
	}

	// A live owner can finish between the sweep's snapshot and this claim.
	record, err := s.executions.get(candidate.RunID)
	if err != nil || record == nil {
		s.releaseRecovery(candidate.RunID)

		if err != nil {
			s.log.Error("failed to reload scheduled execution recovery record", slog.String("run_id", candidate.RunID), logger.ErrAttr(err))
		}

		return
	}

	job := record.Job.scheduled()
	if record.Context != s.contextName || record.Mode != s.mode || job.key == "" {
		s.releaseRecovery(candidate.RunID)
		s.log.Warn("ignoring scheduled execution recovery record with invalid worker identity", slog.String("run_id", record.RunID))

		return
	}

	s.seedRecoveredStopHolds(record, job)
	s.setRunInProgress(job.key, true)

	s.runs.Go(func() {
		defer s.setRunInProgress(job.key, false)
		defer s.releaseRecovery(record.RunID)

		s.recoverExecution(ctx, record, job)
	})
}

// recoverExecution continues monitoring a claimed scheduled execution record until completion.
// It restores any stopped services, waits for the recovered artifact, and updates metrics.
func (s *scheduler) recoverExecution(ctx context.Context, record *executionRecord, job scheduledJob) {
	stackName := getJobStackName(job)
	cfg := record.Finalization.scheduleConfig()
	metricLabels := getScheduledRunMetricLabels(job, cfg, stackName)

	var (
		runErr   error
		runStart *time.Time
		artifact recoveredArtifact
	)

	if !record.Reported {
		var err error

		artifact, err = s.inspectRecoveredArtifact(ctx, record)
		if err != nil {
			// Recovery must still restore stopped services and finalize the record.
			// Only the run history entry or the timeout enforcement is lost.
			s.log.Warn("failed to inspect recovered execution artifact", slog.String("run_id", record.RunID), logger.ErrAttr(err))
		}

		if artifact.startedAt != nil {
			runStart = artifact.startedAt
			s.startTrackedRun(job, record.RunID, *runStart)

			defer func() {
				s.finishTrackedRun(record.RunID, &runErr, recover())
			}()
		} else {
			// Without the original time, recovery cannot order this run against newer executions.
			now := time.Now()
			runStart = &now
		}
	}

	unlockStacks := lockStacks(s.contextName, append([]string{stackName}, resolveStopServiceStacks(cfg.StopServices, stackName)...)...)
	defer unlockStacks()

	if !record.Reported {
		prometheus.ScheduledRunsActive.WithLabelValues(metricLabels...).Inc()

		runErr = s.waitForRecoveredArtifactWithTimeout(ctx, record, job, artifact)

		prometheus.ScheduledRunsActive.WithLabelValues(metricLabels...).Dec()

		// Leave ownership and finalization state untouched on worker shutdown so
		// the next process can adopt the same retained artifact.
		if ctx.Err() != nil {
			runErr = errors.Join(runErr, ctx.Err())
			return
		}
	}

	if len(cfg.StopServices) > 0 && !record.Restored {
		if err := s.startServicesForJob(context.WithoutCancel(ctx), job.mode, getJobStackName(job), cfg.StopServices); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("restore services after recovered execution: %w", err))
			s.log.Error("failed to restore services for recovered scheduled execution", slog.String("run_id", record.RunID), logger.ErrAttr(err))

			return
		}

		record.Restored = true
		if err := s.executions.update(*record); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("persist recovered execution restoration: %w", err))
			s.log.Error("failed to persist recovered scheduled execution restoration state", slog.String("run_id", record.RunID), logger.ErrAttr(err))

			return
		}
	}

	if !record.Reported {
		prometheus.ScheduledRunDuration.WithLabelValues(metricLabels...).Observe(time.Since(*runStart).Seconds())
		prometheus.ScheduledRunsTotal.WithLabelValues(metricLabels...).Inc()

		if runErr != nil {
			prometheus.ScheduledRunErrorsTotal.WithLabelValues(metricLabels...).Inc()
		}

		s.runtime.updateRunStatus(job, cfg, runErr)

		if runErr != nil {
			s.sendRunNotification(job, cfg, record.RunID, false, "Scheduled job failed", fmt.Sprintf("scheduled job '%s' failed to run: %v", job.name, runErr))
		} else {
			s.sendRunNotification(job, cfg, record.RunID, true, "Scheduled job completed", fmt.Sprintf("scheduled job '%s' completed successfully", job.name))
		}
	}

	s.completeExecutionRecord(ctx, record)
}

// recoveredArtifact holds the state of a recovered one-off artifact.
type recoveredArtifact struct {
	// startedAt is the run start time from the job labels. The run history uses it.
	startedAt *time.Time
	// workloadStartedAt is the start time of the workload. The job timeout counts from this time.
	workloadStartedAt *time.Time
	// timeout is the job timeout from the artifact labels. Zero means no timeout.
	timeout time.Duration
	// active is true while the workload still runs.
	active bool
}

// inspectRecoveredArtifact reads the recovery state from the retained Docker artifact.
// It returns an empty artifact if the artifact is missing. On an error, the returned
// artifact keeps all values that recovery could read before the error.
func (s *scheduler) inspectRecoveredArtifact(ctx context.Context, record *executionRecord) (recoveredArtifact, error) {
	var (
		artifact       recoveredArtifact
		labels         map[string]string
		swarmServiceID string
	)

	switch record.Mode {
	case scheduledJobModeContainer:
		containerID, err := docker.FindOneOffContainer(ctx, s.dockerCli.Client(), record.RunID)
		if err != nil {
			return artifact, err
		}

		if containerID == "" {
			return artifact, nil
		}

		result, err := s.dockerCli.Client().ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
		if err != nil {
			return artifact, err
		}

		if result.Container.Config == nil {
			return artifact, errors.New("recovered container configuration is missing")
		}

		labels = result.Container.Config.Labels

		if state := result.Container.State; state != nil {
			artifact.active = state.Running
			artifact.workloadStartedAt = parseContainerStartedAt(state.StartedAt)
		}
	case scheduledJobModeSwarm:
		serviceID, err := docker.FindSwarmOneOffService(ctx, s.dockerCli, record.RunID)
		if err != nil {
			return artifact, err
		}

		if serviceID == "" {
			return artifact, nil
		}

		result, err := s.dockerCli.Client().ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
		if err != nil {
			return artifact, err
		}

		labels = docker.SwarmJobLabels(result.Service)

		swarmServiceID = serviceID

		if !result.Service.CreatedAt.IsZero() {
			artifact.workloadStartedAt = new(result.Service.CreatedAt.UTC())
		}
	default:
		return artifact, fmt.Errorf("unsupported scheduled execution mode %q", record.Mode)
	}

	artifact.startedAt = parseRFC3339Time(labels[docker.DocoCDJobLabels.JobStartedAt])
	if artifact.workloadStartedAt == nil {
		artifact.workloadStartedAt = artifact.startedAt
	}

	timeout, err := docker.ParseJobTimeout(labels[docker.DocoCDJobLabels.JobTimeout])
	if err != nil {
		return artifact, fmt.Errorf("parse job timeout of recovered execution: %w", err)
	}

	artifact.timeout = timeout

	// Recovery lists the Swarm tasks only when it must enforce a timeout.
	if swarmServiceID != "" && timeout > 0 {
		active, err := docker.SwarmOneOffServiceActive(ctx, s.dockerCli, swarmServiceID)
		if err != nil {
			artifact.timeout = 0

			return artifact, fmt.Errorf("read tasks of recovered execution, job timeout is not enforced: %w", err)
		}

		artifact.active = active
	}

	return artifact, nil
}

// parseContainerStartedAt parses the start time from a container state.
// Docker reports the zero time for a container that never started, so the function returns nil for it.
func parseContainerStartedAt(raw string) *time.Time {
	startedAt := parseRFC3339Time(raw)
	if startedAt == nil || startedAt.IsZero() {
		return nil
	}

	return startedAt
}

// claimRecovery acquires exclusive local ownership of a run to prevent
// duplicate adoption. Returns true if claimed, false if already held.
func (s *scheduler) claimRecovery(runID string) bool {
	return s.runtime.claimRecovery(runID)
}

func (s *scheduler) releaseRecovery(runID string) {
	s.runtime.releaseRecovery(runID)
}

func (s *scheduler) waitForRecoveredArtifact(ctx context.Context, record *executionRecord) error {
	switch record.Mode {
	case scheduledJobModeContainer:
		resourceID, err := docker.FindOneOffContainer(ctx, s.dockerCli.Client(), record.RunID)
		if err != nil {
			return err
		}

		if resourceID == "" {
			return fmt.Errorf("recover scheduled one-off run %s: container was not found", record.RunID)
		}

		return docker.WaitOnOneOffContainer(ctx, s.dockerCli.Client(), resourceID)
	case scheduledJobModeSwarm:
		resourceID, err := docker.FindSwarmOneOffService(ctx, s.dockerCli, record.RunID)
		if err != nil {
			return err
		}

		if resourceID == "" {
			return fmt.Errorf("recover scheduled one-off run %s: service was not found", record.RunID)
		}

		return docker.WaitOnSwarmOneOffService(ctx, s.dockerCli, resourceID, s.log)
	default:
		return fmt.Errorf("unsupported scheduled execution mode %q", record.Mode)
	}
}

// waitForRecoveredArtifactWithTimeout waits for the recovered artifact and enforces the job timeout.
// The timeout counts from the workload start, so recovery applies only the remaining time.
// If the deadline already passed, recovery stops the job at once.
//
// A job that already finished reports its own result, also when it ran past the deadline
// while doco-cd was down. The timeout applies only to a job that still runs.
func (s *scheduler) waitForRecoveredArtifactWithTimeout(ctx context.Context, record *executionRecord, job scheduledJob, artifact recoveredArtifact) error {
	if artifact.timeout <= 0 || artifact.workloadStartedAt == nil || !artifact.active {
		return s.waitForRecoveredArtifact(ctx, record)
	}

	timeoutErr := &docker.JobTimeoutError{Timeout: artifact.timeout}
	deadline := artifact.workloadStartedAt.Add(artifact.timeout)

	waitCtx, cancel := context.WithDeadlineCause(ctx, deadline, timeoutErr)
	defer cancel()

	err := s.waitForRecoveredArtifact(waitCtx, record)
	if !jobTimedOut(ctx, waitCtx) {
		return err
	}

	runLog := s.log.With(
		slog.String("job", job.name),
		slog.String("mode", string(job.mode)),
		slog.String("job_id", record.RunID),
		slog.Duration("timeout", artifact.timeout),
	)

	runLog.Warn("recovered scheduled job exceeded its timeout, stopping it", logger.ErrAttr(err))

	if stopErr := s.stopOneOffRun(ctx, record.Mode, record.RunID); stopErr != nil {
		runLog.Error("failed to stop recovered scheduled job after timeout", logger.ErrAttr(stopErr))

		return errors.Join(timeoutErr, fmt.Errorf("stop job after timeout: %w", stopErr))
	}

	return timeoutErr
}

// seedRecoveredStopHolds re-acquires stop holds for services from persisted stop
// plans. Restores each service to its original replica count.
func (s *scheduler) seedRecoveredStopHolds(record *executionRecord, job scheduledJob) {
	if record.Restored || len(record.Finalization.StopServices) == 0 {
		return
	}

	stackName := getJobStackName(job)

	plans := make(map[string]executionStopPlan, len(record.StopPlans))
	for _, plan := range record.StopPlans {
		plans[plan.Project+"\x00"+plan.Service] = plan
	}

	for _, ref := range record.Finalization.StopServices {
		project := ref.Project
		if project == "" {
			project = stackName
		}

		s.acquireStopHold(job.mode, project, ref.Service)

		if plan, ok := plans[project+"\x00"+ref.Service]; ok && plan.Replicas > 0 {
			s.setStopHoldReplicas(job.mode, project, ref.Service, plan.Replicas)
		}

		if job.mode == scheduledJobModeContainer && s.stopHoldTracker != nil {
			s.stopHoldTracker.MarkSchedulerStopHeld(s.contextName, project, ref.Service)
		}
	}
}
