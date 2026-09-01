package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

// listJobs returns all jobs discovered on this worker's Docker context,
// optionally filtered by stack name.
func (s *scheduler) listJobs(ctx context.Context, stackName string) ([]JobInfo, error) {
	jobs, err := s.discoverJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to discover scheduled jobs: %w", err)
	}

	now := schedulerNow()
	stackName = strings.TrimSpace(stackName)
	result := make([]JobInfo, 0, len(jobs))
	states := s.runtime.statesSnapshot()
	runStatuses := s.runtime.runStatusesSnapshot()
	runningStates := s.runtime.runningStatesSnapshot()

	for _, job := range jobs {
		stack := getJobStackName(job)
		if stackName != "" && stack != stackName {
			continue
		}

		info := JobInfo{
			Name:       job.name,
			Context:    docker.DisplayContextName(job.context),
			Stack:      stack,
			Mode:       string(job.mode),
			Repository: job.labels[docker.DocoCDLabels.Source.Name],
			Valid:      true,

			LastRunAt:      parseRFC3339Time(job.labels[docker.DocoCDJobLabels.JobLastRun]),
			LabelNextRunAt: parseRFC3339Time(job.labels[docker.DocoCDJobLabels.JobNextRun]),
		}

		// A run is active if either an execution is currently observed (e.g. a
		// running one_off ephemeral container) or the in-process scheduler is
		// mid-run for this job.
		running := job.running || runningStates[job.key]

		cfg, enabled, parseErr := parseJobConfig(job)
		if parseErr != nil {
			info.Valid = false
			info.ScheduleError = parseErr.Error()
			info.Status = formatRunStatus(job.containerState, job.containerStatus)
			result = append(result, info)

			continue
		}

		info.Enabled = enabled
		if !enabled {
			info.Status = statusForScheduledJob(job, cfg, runStatuses[job.key], running)
			result = append(result, info)

			continue
		}

		info.Schedule = cfg.Schedule
		info.ExecutionMode = cfg.ExecutionMode
		info.SkipRunning = cfg.SkipRunning
		info.NotifyOn = cfg.NotifyOn
		info.Replicas = cfg.SwarmReplicas
		info.StopServices = formatStopServiceRefs(cfg.StopServices)

		schedule, scheduleErr := docker.ParseJobScheduleExpression(cfg.Schedule)
		if scheduleErr != nil {
			info.Valid = false
			info.ScheduleError = scheduleErr.Error()
			info.Status = statusForScheduledJob(job, cfg, runStatuses[job.key], running)
			result = append(result, info)

			continue
		}

		nextRun := schedule.Next(now)

		if state, ok := states[job.key]; ok {
			if !state.lastRun.IsZero() {
				info.LastRunAt = new(state.lastRun)
			}

			if !state.nextRun.IsZero() {
				nextRun = state.nextRun
			}
		}

		info.NextRunAt = &nextRun
		info.Status = statusForScheduledJob(job, cfg, runStatuses[job.key], running)

		result = append(result, info)
	}

	return result, nil
}

// triggerNow executes one configured scheduled job immediately on this
// worker's Docker context. Job selection matches by container/service name
// and optional stack name.
func (s *scheduler) triggerNow(ctx context.Context, jobName, stackName string) (string, error) {
	if strings.TrimSpace(jobName) == "" {
		return "", errors.New("job name is required")
	}

	jobs, err := s.discoverJobs(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to discover scheduled jobs: %w", err)
	}

	job, cfg, err := findRunnableJob(jobs, strings.TrimSpace(jobName), strings.TrimSpace(stackName))
	if err != nil {
		return "", err
	}

	runID := id.New()
	stack := getJobStackName(job)
	metricLabels := getScheduledRunMetricLabels(job, cfg, stack)

	runLog := s.log.With(
		slog.String("job_id", runID),
		slog.String("job", job.name),
		slog.String("stack", stack),
		slog.String("mode", string(job.mode)),
		slog.String("execution_mode", string(cfg.ExecutionMode)),
	)

	runLog.Info("triggered scheduled job now")

	runStart := time.Now()
	runFailed := false

	prometheus.ScheduledRunsActive.WithLabelValues(metricLabels...).Inc()
	defer prometheus.ScheduledRunsActive.WithLabelValues(metricLabels...).Dec()
	defer func() {
		prometheus.ScheduledRunDuration.WithLabelValues(metricLabels...).Observe(time.Since(runStart).Seconds())
	}()
	defer prometheus.ScheduledRunsTotal.WithLabelValues(metricLabels...).Inc()
	defer func() {
		if runFailed {
			prometheus.ScheduledRunErrorsTotal.WithLabelValues(metricLabels...).Inc()
		}
	}()

	// Lock the job's own stack plus any stacks referenced by stop_services (see lockStacks).
	lockedStacks := append([]string{stack}, resolveStopServiceStacks(cfg.StopServices, stack)...)
	unlockStacks := lockStacks(s.contextName, lockedStacks...)

	defer unlockStacks()

	s.setRunInProgress(job.key, true)
	defer s.setRunInProgress(job.key, false)

	err = s.executeScheduledRun(ctx, job, cfg)
	s.runtime.updateRunStatus(job, cfg, err)
	s.runtime.setLastRun(job.key, schedulerNow())

	if err != nil {
		runFailed = true

		runLog.Error("scheduled run failed", logger.ErrAttr(err))
		s.sendRunNotification(job, cfg, runID, false, "Scheduled job failed", fmt.Sprintf("scheduled job '%s' failed to run: %v", job.name, err))

		return runID, err
	}

	runLog.Info("scheduled run completed")
	s.sendRunNotification(job, cfg, runID, true, "Scheduled job completed", fmt.Sprintf("scheduled job '%s' completed successfully", job.name))

	return runID, nil
}

func listJobsForModes(ctx context.Context, modes []scheduledJobMode, cc docker.ContextClient, log *slog.Logger, secretProvider secretprovider.SecretProvider, notifier notification.Sender, runtime *runtimeStore, stackName string, composeOptions docker.ScheduledComposeOptions) ([]JobInfo, error) {
	var result []JobInfo

	for _, mode := range modes {
		jobs, err := newSchedulerForMode(cc, mode, log, nil, secretProvider, notifier, nil, runtime, composeOptions).listJobs(ctx, stackName)
		if err != nil {
			return nil, err
		}

		result = append(result, jobs...)
	}

	return result, nil
}

func triggerNowForModes(ctx context.Context, modes []scheduledJobMode, cc docker.ContextClient, log *slog.Logger, jobName, stackName string, secretProvider secretprovider.SecretProvider, notifier notification.Sender, stopHoldTracker ServiceStopHoldTracker, runtime *runtimeStore, composeOptions docker.ScheduledComposeOptions) (string, error) {
	workers := make(map[scheduledJobMode]*scheduler, len(modes))

	var jobs []scheduledJob

	for _, mode := range modes {
		worker := newSchedulerForMode(cc, mode, log, nil, secretProvider, notifier, stopHoldTracker, runtime, composeOptions)

		discovered, err := worker.discoverJobs(ctx)
		if err != nil {
			return "", fmt.Errorf("failed to discover scheduled jobs: %w", err)
		}

		workers[mode] = worker

		jobs = append(jobs, discovered...)
	}

	job, _, err := findRunnableJob(jobs, strings.TrimSpace(jobName), strings.TrimSpace(stackName))
	if err != nil {
		return "", err
	}

	worker, ok := workers[job.mode]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrScheduledJobNotFound, jobName)
	}

	return worker.triggerNow(ctx, jobName, stackName)
}

func findRunnableJob(jobs []scheduledJob, jobName, stackName string) (scheduledJob, docker.JobScheduleConfig, error) {
	var (
		matchedJob scheduledJob
		matchedCfg docker.JobScheduleConfig
		matches    int
	)

	for _, job := range jobs {
		if job.name != jobName {
			continue
		}

		if stackName != "" && getJobStackName(job) != stackName {
			continue
		}

		cfg, enabled, err := parseJobConfig(job)
		if err != nil {
			return scheduledJob{}, docker.JobScheduleConfig{}, fmt.Errorf("job %q has invalid schedule labels: %w", jobName, err)
		}

		if !enabled {
			return scheduledJob{}, docker.JobScheduleConfig{}, ErrScheduledJobDisabled
		}

		matchedJob = job
		matchedCfg = cfg
		matches++
	}

	if matches == 0 {
		return scheduledJob{}, docker.JobScheduleConfig{}, ErrScheduledJobNotFound
	}

	if matches > 1 {
		return scheduledJob{}, docker.JobScheduleConfig{}, ErrScheduledJobAmbiguous
	}

	return matchedJob, matchedCfg, nil
}
