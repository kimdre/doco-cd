package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/go-co-op/gocron/v2"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/id"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/graceful"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/reconciliation"
)

const (
	schedulerEventReconnectDelay = time.Second
	schedulerRefreshRetryDelay   = time.Second
)

var (
	ErrScheduledJobNotFound  = errors.New("scheduled job not found")
	ErrScheduledJobDisabled  = errors.New("scheduled job is disabled")
	ErrScheduledJobAmbiguous = errors.New("multiple scheduled jobs matched, narrow your selection")

	runtimeStatesMu      sync.RWMutex
	runtimeStates        = map[string]scheduledJobState{}
	runtimeRunStatuses   = map[string]string{}
	runtimeRunningStates = map[string]bool{}
)

type scheduledJobMode string

const (
	scheduledJobModeContainer scheduledJobMode = "container"
	scheduledJobModeSwarm     scheduledJobMode = "swarm"
)

type scheduledJob struct {
	key             string
	name            string
	id              string
	mode            scheduledJobMode
	labels          map[string]string
	containerState  string // Docker container state (container mode only), e.g. "running", "exited"
	containerStatus string // Docker container status string (container mode only), e.g. "Exited (0) 2 hours ago"
	running         bool   // An execution is currently active (e.g. a running one_off ephemeral container)
}

type scheduledJobState struct {
	fingerprint string
	schedule    gocron.Cron
	lastRun     time.Time
	nextRun     time.Time
	deployment  string
	cfg         docker.JobScheduleConfig
}

type scheduler struct {
	dockerCli command.Cli
	log       *slog.Logger
	wg        *sync.WaitGroup
	startedAt time.Time

	states map[string]scheduledJobState

	runningMu sync.Mutex
	running   map[string]bool

	// stopHolds reference-counts services currently held stopped by
	// stopServicesForJob/startServicesForJob. It is keyed by (mode, resolved
	// project/stack, service) so that if two concurrent scheduled runs both
	// declare the same target in stop_services, the target is only actually
	// stopped by the first holder and only actually restarted once the last
	// holder releases it — this prevents one run from prematurely restarting
	// a service another concurrent run still needs stopped. For swarm mode,
	// the held state also records the original replica count so it can be
	// restored when the last holder releases it.
	stopHoldsMu sync.Mutex
	stopHolds   map[stopHoldKey]*stopHoldState
}

// stopHoldKey identifies a service that may be concurrently held stopped by
// more than one scheduled job run.
type stopHoldKey struct {
	mode    scheduledJobMode
	project string
	service string
}

// stopHoldState tracks how many concurrent job runs currently hold a service
// stopped, and (for swarm mode) the replica count it should be restored to.
type stopHoldState struct {
	refCount int
	replicas uint64
}

// JobInfo describes one scheduler-managed target and its runtime scheduling status.
type JobInfo struct {
	Name           string                  `json:"name"`
	Enabled        bool                    `json:"enabled"`
	Stack          string                  `json:"stack,omitempty"`
	Mode           string                  `json:"mode"`
	Schedule       string                  `json:"schedule,omitempty"`
	ExecutionMode  docker.JobExecutionMode `json:"execution_mode,omitempty"`
	SkipRunning    bool                    `json:"skip_running"`
	NotifyOn       docker.JobNotifyOn      `json:"notify_on,omitempty"`
	Replicas       uint64                  `json:"replicas,omitempty"`
	StopServices   []string                `json:"stop_services,omitempty"`
	Status         string                  `json:"status,omitempty"`
	LastRunAt      *time.Time              `json:"last_run_at,omitempty"`
	NextRunAt      *time.Time              `json:"next_run_at,omitempty"`
	LabelNextRunAt *time.Time              `json:"label_next_run_at,omitempty"`
	Repository     string                  `json:"repository,omitempty"`
	ScheduleError  string                  `json:"schedule_error,omitempty"`
	Valid          bool                    `json:"valid"`
}

func Start(ctx context.Context, dockerCli command.Cli, log *slog.Logger, wg *sync.WaitGroup) {
	if dockerCli == nil || log == nil || wg == nil {
		return
	}

	s := &scheduler{
		dockerCli: dockerCli,
		log:       log.With(slog.String("component", "scheduler")),
		wg:        wg,
		startedAt: schedulerNow(),
		states:    map[string]scheduledJobState{},
		running:   map[string]bool{},
		stopHolds: map[stopHoldKey]*stopHoldState{},
	}

	s.run(ctx)
}

// ListJobs returns all discovered scheduler jobs, optionally filtered by stack name.
func ListJobs(ctx context.Context, dockerCli command.Cli, stackName string) ([]JobInfo, error) {
	if dockerCli == nil {
		return nil, errors.New("docker cli is required")
	}

	s := &scheduler{dockerCli: dockerCli}

	jobs, err := s.discoverJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to discover scheduled jobs: %w", err)
	}

	now := schedulerNow()
	stackName = strings.TrimSpace(stackName)
	result := make([]JobInfo, 0, len(jobs))
	states := getRuntimeStatesSnapshot()
	runStatuses := getRuntimeRunStatusesSnapshot()
	runningStates := getRuntimeRunningStatesSnapshot()

	for _, job := range jobs {
		stack := getJobStackName(job)
		if stackName != "" && stack != stackName {
			continue
		}

		info := JobInfo{
			Name:       job.name,
			Stack:      stack,
			Mode:       string(job.mode),
			Repository: job.labels[docker.DocoCDLabels.Source.Name],
			Valid:      true,
		}

		info.LastRunAt = parseRFC3339Time(job.labels[docker.DocoCDJobLabels.JobLastRun])
		info.LabelNextRunAt = parseRFC3339Time(job.labels[docker.DocoCDJobLabels.JobNextRun])

		// A run is active if either an execution is currently observed (e.g. a
		// running one_off ephemeral container) or the in-process scheduler is
		// mid-run for this job.
		running := job.running || runningStates[job.key]

		cfg, enabled, parseErr := parseJobConfig(job, s.log)
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

// TriggerNow executes one configured scheduled job immediately.
// Job selection matches by container/service name and optional stack name.
func TriggerNow(ctx context.Context, dockerCli command.Cli, log *slog.Logger, jobName, stackName string) (string, error) {
	if dockerCli == nil {
		return "", errors.New("docker cli is required")
	}

	if strings.TrimSpace(jobName) == "" {
		return "", errors.New("job name is required")
	}

	if log == nil {
		log = slog.Default()
	}

	s := &scheduler{
		dockerCli: dockerCli,
		log:       log.With(slog.String("component", "scheduler")),
		running:   map[string]bool{},
		stopHolds: map[stopHoldKey]*stopHoldState{},
	}

	jobs, err := s.discoverJobs(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to discover scheduled jobs: %w", err)
	}

	job, cfg, err := findRunnableJob(jobs, strings.TrimSpace(jobName), strings.TrimSpace(stackName))
	if err != nil {
		return "", err
	}

	runID := id.GenID()
	stack := getJobStackName(job)
	metricLabels := getScheduledRunMetricLabels(job, cfg, stack)

	runLog := s.log.With(
		slog.String("job_id", runID),
		slog.String("job", job.name),
		slog.String("stack", stack),
		slog.String("mode", string(job.mode)),
		slog.String("execution_mode", string(cfg.ExecutionMode)),
	)

	runLog.Info("triggering scheduled run via API")

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
	unlockStacks := lockStacks(lockedStacks...)

	defer unlockStacks()

	setRuntimeRunInProgress(job.key, true)
	defer setRuntimeRunInProgress(job.key, false)

	err = s.executeScheduledRun(ctx, job, cfg)
	updateRuntimeRunStatus(job, cfg, err)
	setRuntimeLastRun(job.key, schedulerNow())

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

func (s *scheduler) run(ctx context.Context) {
	jobChanges := s.watchJobChanges(ctx)
	timer := time.NewTimer(time.Hour)

	stopTimer(timer)
	defer timer.Stop()

	s.log.Info("starting scheduler")

	nextRun, hasNextRun := s.refreshJobs(ctx, schedulerNow())

	for {
		setTimerToNextRun(timer, schedulerNow(), nextRun, hasNextRun)

		select {
		case <-ctx.Done():
			s.log.Info("scheduler stopped")
			return
		case _, ok := <-jobChanges:
			if !ok {
				jobChanges = nil
				continue
			}

			nextRun, hasNextRun = s.refreshJobs(ctx, schedulerNow())
		case t := <-timer.C:
			nextRun, hasNextRun = s.refreshJobs(ctx, t)
		}
	}
}

func (s *scheduler) refreshJobs(ctx context.Context, now time.Time) (time.Time, bool) {
	if s.startedAt.IsZero() {
		s.startedAt = now
	}

	jobs, err := s.discoverJobs(ctx)
	if err != nil {
		s.log.Error("failed to discover scheduled jobs", logger.ErrAttr(err))
		return now.Add(schedulerRefreshRetryDelay), true
	}

	active := make(map[string]struct{}, len(jobs))
	discoveredByKey := make(map[string]scheduledJob, len(jobs))

	var nearestNextRun time.Time

	for _, job := range jobs {
		discoveredByKey[job.key] = job

		cfg, enabled, parseErr := parseJobConfig(job, s.log)
		if parseErr != nil {
			s.log.Warn("ignoring job with invalid schedule labels",
				slog.String("job", job.name),
				slog.String("mode", string(job.mode)),
				logger.ErrAttr(parseErr),
			)

			continue
		}

		if !enabled {
			continue
		}

		active[job.key] = struct{}{}

		fingerprint := getScheduleFingerprint(cfg)

		deploymentID, _ := getJobDeploymentIdentity(job.labels)

		prevState, ok := s.states[job.key]
		state := prevState

		if !ok || state.fingerprint != fingerprint {
			schedule, scheduleErr := docker.ParseJobScheduleExpression(cfg.Schedule)
			if scheduleErr != nil {
				s.log.Warn("ignoring job with invalid schedule",
					slog.String("job", job.name),
					slog.String("schedule", cfg.Schedule),
					logger.ErrAttr(scheduleErr),
				)

				continue
			}

			state = scheduledJobState{
				fingerprint: fingerprint,
				schedule:    schedule,
				nextRun:     schedule.Next(now),
				deployment:  deploymentID,
				cfg:         cfg,
			}

			s.states[job.key] = state
			s.log.Info("job scheduled",
				slog.String("job", job.name),
				slog.String("mode", string(job.mode)),
				slog.String("schedule", cfg.Schedule),
				slog.String("next_run", state.nextRun.Format(time.RFC3339)),
			)
		}

		if state.deployment != deploymentID {
			state.deployment = deploymentID
			s.states[job.key] = state
		}

		if !now.Before(state.nextRun) {
			scheduledAt := state.nextRun
			state.lastRun = scheduledAt
			state.nextRun = nextScheduledRun(state.schedule, scheduledAt, now)
			s.states[job.key] = state

			s.triggerRun(context.WithoutCancel(ctx), job, state.cfg, scheduledAt)
		}

		if nearestNextRun.IsZero() || state.nextRun.Before(nearestNextRun) {
			nearestNextRun = state.nextRun
		}
	}

	for key := range s.states {
		if _, exists := active[key]; !exists {
			if job, ok := discoveredByKey[key]; ok {
				s.log.Info("job unscheduled",
					slog.String("job", job.name),
					slog.String("stack", getJobStackName(job)),
					slog.String("mode", string(job.mode)),
					slog.String("reason", "disabled"),
				)
			} else {
				s.log.Info("job unscheduled",
					slog.String("job_key", key),
					slog.String("reason", "removed"),
				)
			}

			delete(s.states, key)
		}
	}

	if nearestNextRun.IsZero() {
		nearestNextRun, _ = getNearestNextRun(s.states)
	}

	setRuntimeStatesSnapshot(s.states)

	return nearestNextRun, !nearestNextRun.IsZero()
}

func (s *scheduler) watchJobChanges(ctx context.Context) <-chan struct{} {
	changes := make(chan struct{}, 1)

	graceful.SafeGo(s.wg, s.log, func() {
		defer close(changes)

		for ctx.Err() == nil {
			filters := make(client.Filters)
			if swarm.GetModeEnabled() {
				filters.Add("type", "service")

				for _, action := range []string{"create", "update", "remove"} {
					filters.Add("event", action)
				}
			} else {
				filters.Add("type", "container")

				for _, action := range []string{"create", "start", "rename", "destroy"} {
					filters.Add("event", action)
				}
			}

			eventResult := s.dockerCli.Client().Events(ctx, client.EventsListOptions{Filters: filters})

			reconnect := false
			for !reconnect {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-eventResult.Messages:
					if !ok {
						reconnect = true
						continue
					}

					s.notifyJobChange(changes)
				case err, ok := <-eventResult.Err:
					if !ok {
						reconnect = true
						continue
					}

					if err != nil && ctx.Err() == nil {
						s.log.Debug("scheduler job change listener error", logger.ErrAttr(err))
					}

					reconnect = true
				}
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(schedulerEventReconnectDelay):
			}
		}
	})

	return changes
}

func (s *scheduler) notifyJobChange(changes chan<- struct{}) {
	select {
	case changes <- struct{}{}:
	default:
	}
}

func (s *scheduler) discoverJobs(ctx context.Context) ([]scheduledJob, error) {
	if s.dockerCli == nil {
		return nil, nil
	}

	if swarm.GetModeEnabled() {
		services, err := s.dockerCli.Client().ServiceList(ctx, client.ServiceListOptions{})
		if err != nil {
			return nil, err
		}

		result := make([]scheduledJob, 0, len(services.Items))
		for _, svc := range services.Items {
			// Deployment metadata and job runtime metadata are read from the service
			// spec, but the job configuration itself must come from the task template
			// only, because that is where the rest of the swarm job handling reads it
			// from. Picking up job config labels from the service spec here would
			// schedule services that the deploy path does not treat as jobs.
			labels := docker.SwarmJobLabels(svc)

			result = append(result, scheduledJob{
				key:    "swarm:" + svc.ID,
				name:   svc.Spec.Name,
				id:     svc.Spec.Name,
				mode:   scheduledJobModeSwarm,
				labels: labels,
			})
		}

		return result, nil
	}

	containers, err := s.dockerCli.Client().ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", docker.DocoCDJobLabels.JobEnabled),
	})
	if err != nil {
		return nil, err
	}

	jobByKey := make(map[string]scheduledJob)
	runningEphemeralByKey := make(map[string]bool)

	for _, c := range containers.Items {
		key := containerJobKey(c.ID, c.Labels)

		// One_off runs execute in an ephemeral clone of the source container that
		// carries the same compose project/service labels. It must not be treated
		// as the job itself, but while it is running it signals that the job is
		// currently executing.
		if isEphemeralScheduledContainer(c.Labels) {
			if c.State == container.StateRunning {
				runningEphemeralByKey[key] = true
			}

			continue
		}

		name := strings.TrimPrefix(firstContainerName(c.Names), "/")
		if name == "" {
			name = c.ID[:12]
		}

		existing, exists := jobByKey[key]
		if exists && existing.id != "" && c.State != container.StateRunning {
			continue
		}

		jobByKey[key] = scheduledJob{
			key:             key,
			name:            name,
			id:              c.ID,
			mode:            scheduledJobModeContainer,
			labels:          c.Labels,
			containerState:  string(c.State),
			containerStatus: c.Status,
		}
	}

	result := make([]scheduledJob, 0, len(jobByKey))
	for key, job := range jobByKey {
		job.running = runningEphemeralByKey[key]
		result = append(result, job)
	}

	return result, nil
}

func (s *scheduler) triggerRun(ctx context.Context, job scheduledJob, cfg docker.JobScheduleConfig, now time.Time) {
	stackName := getJobStackName(job)
	metricLabels := getScheduledRunMetricLabels(job, cfg, stackName)

	if cfg.SkipRunning && s.isRunInProgress(job.key) {
		s.log.Warn("skipping scheduled run because previous run is still in progress",
			slog.String("job", job.name),
			slog.String("stack", stackName),
			slog.String("mode", string(job.mode)),
		)

		prometheus.ScheduledRunSkippedTotal.WithLabelValues(append(metricLabels, "still_running")...).Inc()

		return
	}

	s.setRunInProgress(job.key, true)

	graceful.SafeGo(s.wg, s.log, func() {
		defer s.setRunInProgress(job.key, false)

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

		runID := id.GenID()

		runLog := s.log.With(
			slog.String("job_id", runID),
			slog.String("job", job.name),
			slog.String("stack", stackName),
			slog.String("mode", string(job.mode)),
			slog.String("execution_mode", string(cfg.ExecutionMode)),
			slog.String("scheduled_at", now.Format(time.RFC3339)),
		)

		// Lock the job's own stack plus any stacks referenced by stop_services,
		// so a concurrent deployment to a target stack cannot race with it
		// being stopped/restarted around this run (see lockStacks).
		lockedStacks := append([]string{stackName}, resolveStopServiceStacks(cfg.StopServices, stackName)...)

		runLog.Debug("waiting for scheduler/deploy lock(s)", slog.Any("stacks", lockedStacks))
		unlockStacks := lockStacks(lockedStacks...)

		defer unlockStacks()

		runLog.Debug("acquired scheduler/deploy lock(s)")

		runLog.Debug("triggering scheduled run")

		err := s.executeScheduledRun(ctx, job, cfg)
		updateRuntimeRunStatus(job, cfg, err)

		if err != nil {
			runFailed = true

			runLog.Error("scheduled run failed", logger.ErrAttr(err))
			s.sendRunNotification(job, cfg, runID, false, "Scheduled job failed", fmt.Sprintf("scheduled job '%s' failed to run: %v", job.name, err))

			return
		}

		runLog.Info("scheduled run completed", slog.String("next_run", s.states[job.key].nextRun.Format(time.RFC3339)))
		s.sendRunNotification(job, cfg, runID, true, "Scheduled job completed", fmt.Sprintf("scheduled job '%s' completed successfully", job.name))
	})
}

func (s *scheduler) executeScheduledRun(ctx context.Context, job scheduledJob, cfg docker.JobScheduleConfig) error {
	// Stop any declared services before executing the job, then restart them
	// afterwards regardless of whether the job succeeds or fails.
	//
	// The restart is deferred unconditionally, before attempting the stop and
	// regardless of whether it fully succeeds, so that any services which
	// *were* successfully stopped are never left stranded (e.g. if stopping
	// service 2 of 3 fails, service 1 must still be restarted). Restarting an
	// already-running service/container is a harmless no-op.
	if len(cfg.StopServices) > 0 {
		stackName := getJobStackName(job)
		restoreCtx := context.WithoutCancel(ctx)

		defer func() {
			if err := s.startServicesForJob(restoreCtx, job.mode, stackName, cfg.StopServices); err != nil {
				s.log.Error("failed to restart services after scheduled job",
					slog.String("job", job.name),
					logger.ErrAttr(err),
				)
			}
		}()

		if err := s.stopServicesForJob(ctx, job.mode, stackName, cfg.StopServices); err != nil {
			return fmt.Errorf("stopping services before job: %w", err)
		}
	}

	switch job.mode {
	case scheduledJobModeContainer:
		switch cfg.ExecutionMode {
		case docker.JobExecutionModeOneOff:
			err := docker.RunComposeOneOffFromServiceDefinition(ctx, s.dockerCli, job.labels)
			if err == nil {
				return nil
			}

			if !errors.Is(err, docker.ErrComposeScheduledMetadataUnavailable) {
				return err
			}

			return docker.RunContainerOneOffFromExisting(ctx, s.dockerCli.Client(), job.id)
		default:
			err := docker.RunComposeScheduledContainer(ctx, s.dockerCli, job.id, job.labels, len(cfg.StopServices) > 0)
			if err == nil {
				return nil
			}

			if !errors.Is(err, docker.ErrComposeScheduledMetadataUnavailable) {
				return err
			}

			if len(cfg.StopServices) > 0 {
				// stop_services requires knowing when the job has finished.
				// Use the blocking variant so services are not restarted while
				// the job container is still running.
				return docker.RestartContainerAndWait(ctx, s.dockerCli.Client(), job.id)
			}

			return docker.RestartContainer(ctx, s.dockerCli.Client(), job.id)
		}
	case scheduledJobModeSwarm:
		switch cfg.ExecutionMode {
		case docker.JobExecutionModeOneOff:
			return docker.RunSwarmOneOffFromService(ctx, s.dockerCli, job.id, docker.SwarmOneOffFromServiceOptions{
				Replicas:         cfg.SwarmReplicas,
				SendRegistryAuth: true,
			})
		default:
			err := docker.RerunJobService(ctx, s.dockerCli.Client(), job.id)
			if err == nil {
				return nil
			}

			if errors.Is(err, docker.ErrNotAJobService) {
				return docker.RestartScheduledSwarmService(ctx, s.dockerCli, job.id)
			}

			return err
		}
	default:
		return fmt.Errorf("unsupported scheduled job mode %q", job.mode)
	}
}

const stopServicesTimeout = 30 * time.Second

// acquireStopHold registers this run as one of the (possibly several)
// concurrent holders of project/service being stopped, for the given mode.
// It returns true if this is the first holder (i.e. the caller must actually
// perform the stop); subsequent concurrent holders are told the target is
// already held stopped and must not stop it again or restore it until they
// are the last holder to release it.
func (s *scheduler) acquireStopHold(mode scheduledJobMode, project, service string) bool {
	key := stopHoldKey{mode: mode, project: project, service: service}

	s.stopHoldsMu.Lock()
	defer s.stopHoldsMu.Unlock()

	st, ok := s.stopHolds[key]
	if !ok {
		st = &stopHoldState{}
		s.stopHolds[key] = st
	}

	st.refCount++

	return st.refCount == 1
}

// setStopHoldReplicas records the original swarm replica count for a held
// service, so the last holder to release it can restore it.
func (s *scheduler) setStopHoldReplicas(mode scheduledJobMode, project, service string, replicas uint64) {
	key := stopHoldKey{mode: mode, project: project, service: service}

	s.stopHoldsMu.Lock()
	defer s.stopHoldsMu.Unlock()

	if st, ok := s.stopHolds[key]; ok {
		st.replicas = replicas
	}
}

// releaseStopHold releases this run's hold on project/service. It returns
// true (and the recorded replica count, for swarm mode) if this was the last
// holder, meaning the caller must actually perform the restart; otherwise
// another concurrent run still needs the target held stopped.
func (s *scheduler) releaseStopHold(mode scheduledJobMode, project, service string) (isLast bool, replicas uint64) {
	key := stopHoldKey{mode: mode, project: project, service: service}

	s.stopHoldsMu.Lock()
	defer s.stopHoldsMu.Unlock()

	st, ok := s.stopHolds[key]
	if !ok {
		// No recorded hold (shouldn't normally happen) — nothing to restore.
		return false, 0
	}

	st.refCount--
	replicas = st.replicas

	if st.refCount <= 0 {
		delete(s.stopHolds, key)
		return true, replicas
	}

	return false, replicas
}

// stopServicesForJob stops the services listed in StopServices before a job runs.
// For compose mode, services are grouped by project and stopped via the compose API.
// For swarm mode, services are scaled to 0 (global-mode services are skipped with a warning).
//
// If another concurrent scheduled run already holds a given project/service
// stopped (e.g. two jobs both list the same shared dependency), this run
// records an additional hold on it but does not stop it again — see
// acquireStopHold. This prevents the first run's restart from prematurely
// bringing the service back up while the second run still needs it stopped.
//
// This is best-effort: a failure to stop one project/service does not abort
// attempts to stop the others, so as many of the declared services as
// possible are quiesced before the job runs. All failures are aggregated and
// returned together so the job is still not executed if any stop failed.
func (s *scheduler) stopServicesForJob(ctx context.Context, mode scheduledJobMode, jobStack string, refs []docker.StopServiceRef) error {
	var errs []string

	switch mode {
	case scheduledJobModeContainer:
		byProject := groupStopRefsByProject(refs, jobStack)
		for project, services := range byProject {
			var toStop []string

			for _, svc := range services {
				if s.acquireStopHold(mode, project, svc) {
					toStop = append(toStop, svc)
				} else {
					s.log.Debug("service already held stopped by another concurrent job, skipping duplicate stop",
						slog.String("project", project),
						slog.String("service", svc),
					)
				}
			}

			if len(toStop) == 0 {
				continue
			}

			// Mark each service as scheduler-held so that the reconciliation
			// event listener does not restart it while the job is running.
			for _, svc := range toStop {
				reconciliation.MarkSchedulerStopHeld(project, svc)
			}

			s.log.Info("stopping services before scheduled job",
				slog.String("project", project),
				slog.Any("services", toStop),
			)

			if err := docker.StopProjectServices(ctx, s.dockerCli, project, toStop, stopServicesTimeout); err != nil {
				errs = append(errs, fmt.Sprintf("project %q services %v: %v", project, toStop, err))
			}
		}

	case scheduledJobModeSwarm:
		for _, ref := range refs {
			stack := ref.Project
			if stack == "" {
				stack = jobStack
			}

			fullName := stack + "_" + ref.Service

			if !s.acquireStopHold(mode, stack, ref.Service) {
				s.log.Debug("swarm service already held stopped by another concurrent job, skipping duplicate stop",
					slog.String("service", fullName),
				)

				continue
			}

			replicas, err := docker.StopSwarmService(ctx, s.dockerCli, fullName, stopServicesTimeout)

			switch {
			case errors.Is(err, docker.ErrGlobalSwarmServiceNotScalable):
				s.log.Warn("skipping global-mode swarm service in stop_services (cannot scale to 0)",
					slog.String("service", fullName),
				)

				continue

			case errors.Is(err, docker.ErrSwarmServiceAlreadyStopped):
				s.log.Info("swarm service in stop_services is already scaled to 0, nothing to stop",
					slog.String("service", fullName),
				)

				continue

			case err != nil:
				// The scale-down may have been applied even though the call failed
				// (e.g. waiting for tasks to terminate timed out). Whenever a
				// replica count was reported, record it so the service is still
				// restored afterwards instead of being stranded at 0 replicas.
				if replicas > 0 {
					s.setStopHoldReplicas(mode, stack, ref.Service, replicas)
				}

				errs = append(errs, fmt.Sprintf("swarm service %q: %v", fullName, err))

				continue
			}

			s.log.Info("stopped swarm service before scheduled job",
				slog.String("service", fullName),
				slog.Uint64("original_replicas", replicas),
			)

			// Record the replica count so the last holder to release it can restore it.
			s.setStopHoldReplicas(mode, stack, ref.Service, replicas)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to stop %d service group(s): %s", len(errs), strings.Join(errs, "; "))
	}

	return nil
}

// startServicesForJob restarts services that were stopped by stopServicesForJob.
// It is always called in a deferred block (using context.WithoutCancel) so services
// are restarted even if the job itself fails.
//
// A service is only actually restarted once every concurrent holder of it has
// released their hold (see releaseStopHold) — if another concurrent run is
// still relying on the service being stopped, this run's release is a no-op.
func (s *scheduler) startServicesForJob(ctx context.Context, mode scheduledJobMode, jobStack string, refs []docker.StopServiceRef) error {
	var errs []string

	switch mode {
	case scheduledJobModeContainer:
		byProject := groupStopRefsByProject(refs, jobStack)
		for project, services := range byProject {
			var toStart []string

			for _, svc := range services {
				if isLast, _ := s.releaseStopHold(mode, project, svc); isLast {
					toStart = append(toStart, svc)
				} else {
					s.log.Debug("service still held stopped by another concurrent job, deferring restart",
						slog.String("project", project),
						slog.String("service", svc),
					)
				}
			}

			if len(toStart) == 0 {
				continue
			}

			// Unmark the scheduler hold before starting — if our start fails,
			// reconciliation can step in and recover the service.
			for _, svc := range toStart {
				reconciliation.UnmarkSchedulerStopHeld(project, svc)
			}

			s.log.Info("restarting services after scheduled job",
				slog.String("project", project),
				slog.Any("services", toStart),
			)

			if err := docker.StartProjectServices(ctx, s.dockerCli, project, toStart); err != nil {
				errs = append(errs, fmt.Sprintf("project %q services %v: %v", project, toStart, err))
			}
		}

	case scheduledJobModeSwarm:
		for _, ref := range refs {
			stack := ref.Project
			if stack == "" {
				stack = jobStack
			}

			fullName := stack + "_" + ref.Service

			isLast, replicas := s.releaseStopHold(mode, stack, ref.Service)
			if !isLast {
				s.log.Debug("swarm service still held stopped by another concurrent job, deferring restart",
					slog.String("service", fullName),
				)

				continue
			}

			if replicas == 0 {
				// Was a global-mode service (skipped during stop), or was never
				// actually stopped by us in the first place — nothing to restore.
				continue
			}

			s.log.Info("restarting swarm service after scheduled job",
				slog.String("service", fullName),
				slog.Uint64("replicas", replicas),
			)

			if err := docker.StartSwarmService(ctx, s.dockerCli, fullName, replicas); err != nil {
				errs = append(errs, fmt.Sprintf("swarm service %q: %v", fullName, err))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to restart %d service(s): %s", len(errs), strings.Join(errs, "; "))
	}

	return nil
}

// lockStacks acquires the per-stack scheduler/deploy lock (see lock.LockStack)
// for every distinct, non-empty stack name given, in a deterministic (sorted)
// order. Locking multiple stacks in a fixed global order — rather than in
// caller-supplied order — prevents ABBA deadlocks when two scheduled runs
// need to lock an overlapping set of stacks concurrently (e.g. a job's own
// stack plus stacks referenced by its stop_services, which another run might
// need to lock in the opposite order). It returns an unlock function that
// releases all acquired locks; callers must call it exactly once.
func lockStacks(stacks ...string) (unlock func()) {
	seen := make(map[string]struct{}, len(stacks))
	unique := make([]string, 0, len(stacks))

	for _, stack := range stacks {
		stack = strings.TrimSpace(stack)
		if stack == "" {
			continue
		}

		if _, ok := seen[stack]; ok {
			continue
		}

		seen[stack] = struct{}{}
		unique = append(unique, stack)
	}

	sort.Strings(unique)

	for _, stack := range unique {
		lock.LockStack(stack)
	}

	return func() {
		for i := len(unique) - 1; i >= 0; i-- {
			lock.UnlockStack(unique[i])
		}
	}
}

// resolveStopServiceStacks returns the distinct resolved stack/project names
// referenced by refs, using defaultStack for entries with no explicit project.
// Used to also lock any stacks targeted by stop_services (in addition to the
// job's own stack) so a concurrent deployment to a target stack cannot race
// with it being stopped/restarted around the scheduled run.
func resolveStopServiceStacks(refs []docker.StopServiceRef, defaultStack string) []string {
	stacks := make([]string, 0, len(refs))

	for _, ref := range refs {
		stack := ref.Project
		if stack == "" {
			stack = defaultStack
		}

		stacks = append(stacks, stack)
	}

	return stacks
}

// groupStopRefsByProject groups StopServiceRef slices by their resolved project name.
func groupStopRefsByProject(refs []docker.StopServiceRef, defaultProject string) map[string][]string {
	byProject := make(map[string][]string)

	for _, ref := range refs {
		project := ref.Project
		if project == "" {
			project = defaultProject
		}

		byProject[project] = append(byProject[project], ref.Service)
	}

	return byProject
}

// formatStopServiceRefs serialises StopServiceRef values into the canonical
// "project/service" or "service" strings used in labels and JobInfo.
func formatStopServiceRefs(refs []docker.StopServiceRef) []string {
	if len(refs) == 0 {
		return nil
	}

	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.Project != "" {
			out = append(out, r.Project+"/"+r.Service)
		} else {
			out = append(out, r.Service)
		}
	}

	return out
}

func (s *scheduler) sendRunNotification(job scheduledJob, cfg docker.JobScheduleConfig, runID string, success bool, title, msg string) {
	shouldSend := cfg.ShouldNotifyFailure()
	lvl := notification.Failure

	if success {
		shouldSend = cfg.ShouldNotifySuccess()
		lvl = notification.Success
	}

	if !shouldSend {
		return
	}

	actorKind := "container"
	if job.mode == scheduledJobModeSwarm {
		actorKind = "service"
	}

	metadata := notification.Metadata{
		Repository:        job.labels[docker.DocoCDLabels.Source.Name],
		Stack:             job.labels[docker.DocoCDLabels.Deployment.Name],
		Target:            job.labels[docker.DocoCDLabels.Deployment.ConfigTarget],
		Revision:          notification.GetRevision("", job.labels[docker.DocoCDLabels.Deployment.CommitSHA]),
		JobID:             runID,
		AffectedActorKind: actorKind,
		AffectedActorName: job.name,
	}

	if err := notification.Send(lvl, title, msg, metadata); err != nil {
		s.log.Error("failed to send scheduled job notification", logger.ErrAttr(err), slog.String("job", job.name))
	}
}

func (s *scheduler) isRunInProgress(key string) bool {
	s.runningMu.Lock()
	defer s.runningMu.Unlock()

	return s.running[key]
}

func (s *scheduler) setRunInProgress(key string, inProgress bool) {
	s.runningMu.Lock()
	defer s.runningMu.Unlock()

	setRuntimeRunInProgress(key, inProgress)

	if inProgress {
		s.running[key] = true
		return
	}

	delete(s.running, key)
}

func getScheduleFingerprint(cfg docker.JobScheduleConfig) string {
	return strings.Join([]string{
		cfg.Schedule,
		string(cfg.ExecutionMode),
		strconv.FormatBool(cfg.SkipRunning),
		string(cfg.NotifyOn),
		strconv.FormatUint(cfg.SwarmReplicas, 10),
		strings.Join(formatStopServiceRefs(cfg.StopServices), ","),
	}, "|")
}

// getJobDeploymentIdentity returns a string identifying the deployment of the job and its timestamp.
func getJobDeploymentIdentity(labels map[string]string) (string, time.Time) {
	deploymentID := strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.Timestamp])
	if deploymentID == "" {
		deploymentID = strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.ComposeHash])
	}

	if deploymentID == "" {
		deploymentID = strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.CommitSHA])
	}

	deploymentAt := parseRFC3339Time(labels[docker.DocoCDLabels.Deployment.Timestamp])
	if deploymentAt == nil {
		return deploymentID, time.Time{}
	}

	return deploymentID, *deploymentAt
}

func shouldStopContainerForOneOffDeployRun(job scheduledJob, cfg docker.JobScheduleConfig) bool {
	return job.mode == scheduledJobModeContainer && cfg.ExecutionMode == docker.JobExecutionModeOneOff
}

func getScheduledRunMetricLabels(job scheduledJob, cfg docker.JobScheduleConfig, stackName string) []string {
	return []string{stackName, job.name, string(job.mode), string(cfg.ExecutionMode)}
}

func nextScheduledRun(schedule gocron.Cron, scheduledAt, now time.Time) time.Time {
	nextRun := schedule.Next(scheduledAt)
	for !now.Before(nextRun) {
		nextRun = schedule.Next(nextRun)
	}

	return nextRun
}

func getNearestNextRun(states map[string]scheduledJobState) (time.Time, bool) {
	var nearest time.Time

	for _, state := range states {
		if state.nextRun.IsZero() {
			continue
		}

		if nearest.IsZero() || state.nextRun.Before(nearest) {
			nearest = state.nextRun
		}
	}

	return nearest, !nearest.IsZero()
}

func setTimerToNextRun(timer *time.Timer, now, nextRun time.Time, enabled bool) {
	stopTimer(timer)

	if !enabled {
		return
	}

	delay := time.Until(nextRun)
	if !nextRun.IsZero() {
		delay = nextRun.Sub(now)
	}

	if delay < 0 {
		delay = 0
	}

	timer.Reset(delay)
}

func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}

	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func getJobStackName(job scheduledJob) string {
	if stack := strings.TrimSpace(job.labels[docker.DocoCDLabels.Deployment.Name]); stack != "" {
		return stack
	}

	if stack := strings.TrimSpace(job.labels[swarm.StackNamespaceLabel]); stack != "" {
		return stack
	}

	if stack := strings.TrimSpace(job.labels[api.ProjectLabel]); stack != "" {
		return stack
	}

	return ""
}

// jobOwnIdentity resolves the project/stack and service name that identify
// job itself, used to detect stop_services self-references for both compose
// and Swarm jobs.
//
// Compose containers always carry com.docker.compose.project/.service labels
// (Docker injects them at container-create time), so those are used directly.
// Swarm services deployed by doco-cd do not carry those labels on the task
// spec, so the service name is derived from the full Swarm service name
// (e.g. "mystack_myservice"), stripping the resolved stack prefix.
func jobOwnIdentity(job scheduledJob) (project, service string) {
	project = getJobStackName(job)

	if job.mode == scheduledJobModeSwarm {
		service = strings.TrimPrefix(job.name, project+"_")
		return project, service
	}

	return project, strings.TrimSpace(job.labels[api.ServiceLabel])
}

// parseJobConfig parses a job's schedule labels and, if stop_services is set,
// validates that the job does not reference itself. Centralising this here
// (rather than in docker.ParseJobScheduleLabels) lets the self-reference
// check use the correct own-identity resolution for both compose and Swarm
// jobs; see jobOwnIdentity.
func parseJobConfig(job scheduledJob, log ...*slog.Logger) (docker.JobScheduleConfig, bool, error) {
	cfg, enabled, err := docker.ParseJobScheduleLabels(job.labels, log...)
	if err != nil || !enabled || len(cfg.StopServices) == 0 {
		return cfg, enabled, err
	}

	// In Swarm mode, stop_services requires one_off because there is no
	// reliable completion signal for restart-mode services (ScaleService /
	// ServiceUpdate only record intent and return immediately).
	if job.mode == scheduledJobModeSwarm && cfg.ExecutionMode != docker.JobExecutionModeOneOff {
		return cfg, false, fmt.Errorf(
			"%s requires %s=%q in Swarm mode (got %q): doco-cd cannot detect job completion for Swarm services in %q mode",
			docker.DocoCDJobLabels.JobStopServices, docker.DocoCDJobLabels.JobExecutionMode,
			docker.JobExecutionModeOneOff, cfg.ExecutionMode, cfg.ExecutionMode,
		)
	}

	project, service := jobOwnIdentity(job)
	if err := docker.ValidateStopServicesSelfReference(project, service, cfg.StopServices); err != nil {
		return cfg, false, err
	}

	return cfg, enabled, nil
}

func firstContainerName(names []string) string {
	if len(names) == 0 {
		return ""
	}

	return names[0]
}

func isEphemeralScheduledContainer(labels map[string]string) bool {
	if labels == nil {
		return false
	}

	raw, ok := labels[docker.DocoCDJobLabels.JobEphemeral]
	if !ok {
		return false
	}

	isEphemeral, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}

	return isEphemeral
}

// containerJobKey derives the stable scheduler key for a container from its
// compose project/service labels, falling back to the container ID. An ephemeral
// one_off clone shares the source's project/service labels, so it resolves to the
// same key as its source job.
func containerJobKey(containerID string, labels map[string]string) string {
	service := labels[api.ServiceLabel]
	project := labels[api.ProjectLabel]

	if project != "" && service != "" {
		return "container:" + project + "/" + service
	}

	return "container:" + containerID
}

func formatRunStatus(state, status string) string {
	state = strings.TrimSpace(state)
	if state != string(container.StateExited) {
		return state
	}

	status = strings.TrimSpace(status)

	start := strings.Index(status, "(")
	if start < 0 {
		return state
	}

	end := strings.Index(status[start:], ")")
	if end <= 0 {
		return state
	}

	code := strings.TrimSpace(status[start+1 : start+end])
	if code == "" {
		return state
	}

	return state + " (" + code + ")"
}

// formatExitStatus renders an exited container status with its exit code,
// matching the format produced by formatRunStatus (e.g. "exited (0)").
func formatExitStatus(code int) string {
	return fmt.Sprintf("%s (%d)", container.StateExited, code)
}

func statusForScheduledJob(job scheduledJob, cfg docker.JobScheduleConfig, runtimeStatus string, running bool) string {
	if running {
		return string(container.StateRunning)
	}

	status := formatRunStatus(job.containerState, job.containerStatus)

	if job.mode != scheduledJobModeContainer || cfg.ExecutionMode != docker.JobExecutionModeOneOff {
		return status
	}

	if strings.TrimSpace(job.containerState) != string(container.StateCreated) {
		return status
	}

	runtimeStatus = strings.TrimSpace(runtimeStatus)
	if runtimeStatus == "" {
		return status
	}

	return runtimeStatus
}

func parseRFC3339Time(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}

	return new(t.UTC())
}

// schedulerNow returns the current time in local timezone for consistent scheduling behavior regardless of host timezone settings.
func schedulerNow() time.Time {
	return time.Now().In(time.Local)
}

func setRuntimeStatesSnapshot(states map[string]scheduledJobState) {
	runtimeStatesMu.Lock()
	defer runtimeStatesMu.Unlock()

	runtimeStates = copyMapLocked(states)
}

func getRuntimeStatesSnapshot() map[string]scheduledJobState {
	runtimeStatesMu.RLock()
	defer runtimeStatesMu.RUnlock()

	return copyMapLocked(runtimeStates)
}

func setRuntimeLastRun(key string, lastRun time.Time) {
	if key == "" {
		return
	}

	runtimeStatesMu.Lock()
	defer runtimeStatesMu.Unlock()

	state := runtimeStates[key]
	state.lastRun = lastRun
	runtimeStates[key] = state
}

func getRuntimeRunStatusesSnapshot() map[string]string {
	runtimeStatesMu.RLock()
	defer runtimeStatesMu.RUnlock()

	return copyMapLocked(runtimeRunStatuses)
}

func setRuntimeRunStatus(key, status string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}

	runtimeStatesMu.Lock()
	defer runtimeStatesMu.Unlock()

	runtimeRunStatuses[key] = strings.TrimSpace(status)
}

func getRuntimeRunningStatesSnapshot() map[string]bool {
	runtimeStatesMu.RLock()
	defer runtimeStatesMu.RUnlock()

	return copyMapLocked(runtimeRunningStates)
}

func setRuntimeRunInProgress(key string, inProgress bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}

	runtimeStatesMu.Lock()
	defer runtimeStatesMu.Unlock()

	if inProgress {
		runtimeRunningStates[key] = true
		return
	}

	delete(runtimeRunningStates, key)
}

func updateRuntimeRunStatus(job scheduledJob, cfg docker.JobScheduleConfig, runErr error) {
	if job.mode != scheduledJobModeContainer || cfg.ExecutionMode != docker.JobExecutionModeOneOff {
		return
	}

	if runErr == nil {
		setRuntimeRunStatus(job.key, formatExitStatus(0))
		return
	}

	var exitErr *docker.ContainerExitError
	if errors.As(runErr, &exitErr) {
		setRuntimeRunStatus(job.key, formatExitStatus(exitErr.ExitCode))
	}
}

// copyMapLocked returns a shallow copy of m. Callers must hold runtimeStatesMu.
func copyMapLocked[K comparable, V any](m map[K]V) map[K]V {
	ret := make(map[K]V, len(m))
	maps.Copy(ret, m)

	return ret
}
