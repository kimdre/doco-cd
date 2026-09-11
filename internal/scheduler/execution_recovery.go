package scheduler

import (
	"context"
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

		return docker.RemoveSwarmOneOffService(ctx, s.dockerCli, resourceID)
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
		job := record.Job.scheduled()
		if job.key == "" {
			continue
		}

		if !s.claimRecovery(record.RunID) {
			continue
		}

		s.seedRecoveredStopHolds(&record, job)
		s.setRunInProgress(job.key, true)

		s.runs.Go(func() {
			defer s.setRunInProgress(job.key, false)
			defer s.releaseRecovery(record.RunID)

			s.recoverExecution(ctx, &record, job)
		})
	}
}

func (s *scheduler) recoverExecution(ctx context.Context, record *executionRecord, job scheduledJob) {
	stackName := getJobStackName(job)
	cfg := record.Finalization.scheduleConfig()
	metricLabels := getScheduledRunMetricLabels(job, cfg, stackName)

	unlockStacks := lockStacks(s.contextName, append([]string{stackName}, resolveStopServiceStacks(cfg.StopServices, stackName)...)...)
	defer unlockStacks()

	var (
		runErr   error
		runStart *time.Time
	)

	if !record.Reported {
		// Extract original start time from artifact labels for accurate metrics.
		// If artifact was removed, use current time instead.
		runStart = s.recoveredExecutionStartedAt(ctx, record)

		if runStart == nil {
			now := time.Now()
			runStart = &now
		}

		prometheus.ScheduledRunsActive.WithLabelValues(metricLabels...).Inc()

		runErr = s.waitForRecoveredArtifact(ctx, record)

		prometheus.ScheduledRunsActive.WithLabelValues(metricLabels...).Dec()

		// Leave ownership and finalization state untouched on worker shutdown so
		// the next process can adopt the same retained artifact.
		if ctx.Err() != nil {
			return
		}
	}

	if len(cfg.StopServices) > 0 && !record.Restored {
		if err := s.startServicesForJob(context.WithoutCancel(ctx), job.mode, getJobStackName(job), cfg.StopServices); err != nil {
			s.log.Error("failed to restore services for recovered scheduled execution", slog.String("run_id", record.RunID), logger.ErrAttr(err))
			return
		}

		record.Restored = true
		if err := s.executions.update(*record); err != nil {
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

// recoveredExecutionStartedAt reads the start time from Docker artifact labels.
// Returns nil if artifact or label is missing.
func (s *scheduler) recoveredExecutionStartedAt(ctx context.Context, record *executionRecord) *time.Time {
	var labels map[string]string

	switch record.Mode {
	case scheduledJobModeContainer:
		containerID, err := docker.FindOneOffContainer(ctx, s.dockerCli.Client(), record.RunID)
		if err != nil || containerID == "" {
			return nil
		}

		result, err := s.dockerCli.Client().ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
		if err != nil || result.Container.Config == nil {
			return nil
		}

		labels = result.Container.Config.Labels
	case scheduledJobModeSwarm:
		serviceID, err := docker.FindSwarmOneOffService(ctx, s.dockerCli, record.RunID)
		if err != nil || serviceID == "" {
			return nil
		}

		result, err := s.dockerCli.Client().ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
		if err != nil {
			return nil
		}

		labels = docker.SwarmJobLabels(result.Service)
	}

	return parseRFC3339Time(labels[docker.DocoCDJobLabels.JobStartedAt])
}

// claimRecovery acquires exclusive local ownership of a run to prevent
// duplicate adoption. Returns true if claimed, false if already held.
func (s *scheduler) claimRecovery(runID string) bool {
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()

	if s.recovering.Contains(runID) {
		return false
	}

	s.recovering.Add(runID)

	return true
}

func (s *scheduler) releaseRecovery(runID string) {
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()

	delete(s.recovering, runID)
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

		return docker.WaitOnSwarmOneOffService(ctx, s.dockerCli, resourceID)
	default:
		return fmt.Errorf("unsupported scheduled execution mode %q", record.Mode)
	}
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
