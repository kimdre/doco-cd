package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/robfig/cron/v3"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/graceful"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/utils/id"
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
	exitCodeMatcher      = regexp.MustCompile(`exited with status (\d+)`)
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
}

type scheduledJobState struct {
	fingerprint string
	schedule    cron.Schedule
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

		cfg, enabled, parseErr := docker.ParseJobScheduleLabels(job.labels, s.log)
		if parseErr != nil {
			info.Valid = false
			info.ScheduleError = parseErr.Error()
			info.Status = formatRunStatus(job.containerState, job.containerStatus)
			result = append(result, info)

			continue
		}

		info.Enabled = enabled
		if !enabled {
			info.Status = statusForScheduledJob(job, cfg, runStatuses[job.key], runningStates[job.key])
			result = append(result, info)

			continue
		}

		info.Schedule = cfg.Schedule
		info.ExecutionMode = cfg.ExecutionMode
		info.SkipRunning = cfg.SkipRunning
		info.NotifyOn = cfg.NotifyOn
		info.Replicas = cfg.SwarmReplicas

		schedule, scheduleErr := docker.ParseJobScheduleExpression(cfg.Schedule)
		if scheduleErr != nil {
			info.Valid = false
			info.ScheduleError = scheduleErr.Error()
			info.Status = statusForScheduledJob(job, cfg, runStatuses[job.key], runningStates[job.key])
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
		info.Status = statusForScheduledJob(job, cfg, runStatuses[job.key], runningStates[job.key])

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

	lock.LockStack(stack)

	defer lock.UnlockStack(stack)

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

		cfg, enabled, err := docker.ParseJobScheduleLabels(job.labels)
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

		cfg, enabled, parseErr := docker.ParseJobScheduleLabels(job.labels, s.log)
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
			labels := map[string]string{}
			if svc.Spec.TaskTemplate.ContainerSpec != nil && svc.Spec.TaskTemplate.ContainerSpec.Labels != nil {
				labels = svc.Spec.TaskTemplate.ContainerSpec.Labels
			}

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

	for _, c := range containers.Items {
		if isEphemeralScheduledContainer(c.Labels) {
			continue
		}

		name := strings.TrimPrefix(firstContainerName(c.Names), "/")
		if name == "" {
			name = c.ID[:12]
		}

		service := c.Labels[api.ServiceLabel]
		project := c.Labels[api.ProjectLabel]

		key := "container:" + c.ID
		if project != "" && service != "" {
			key = "container:" + project + "/" + service
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
	for _, job := range jobByKey {
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

		runLog.Debug("waiting for scheduler/deploy lock")
		lock.LockStack(stackName)

		defer lock.UnlockStack(stackName)

		runLog.Debug("acquired scheduler/deploy lock")

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
	switch job.mode {
	case scheduledJobModeContainer:
		switch cfg.ExecutionMode {
		case docker.JobExecutionModeOneOff:
			return docker.RunContainerOneOffFromExisting(ctx, s.dockerCli.Client(), job.id)
		default:
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

func nextScheduledRun(schedule cron.Schedule, scheduledAt, now time.Time) time.Time {
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

	next := make(map[string]scheduledJobState, len(states))
	maps.Copy(next, states)

	runtimeStates = next
}

func getRuntimeStatesSnapshot() map[string]scheduledJobState {
	runtimeStatesMu.RLock()
	defer runtimeStatesMu.RUnlock()

	ret := make(map[string]scheduledJobState, len(runtimeStates))
	maps.Copy(ret, runtimeStates)

	return ret
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

	ret := make(map[string]string, len(runtimeRunStatuses))
	maps.Copy(ret, runtimeRunStatuses)

	return ret
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

	ret := make(map[string]bool, len(runtimeRunningStates))
	maps.Copy(ret, runtimeRunningStates)

	return ret
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
		setRuntimeRunStatus(job.key, "exited (0)")
		return
	}

	matches := exitCodeMatcher.FindStringSubmatch(runErr.Error())
	if len(matches) != 2 {
		return
	}

	code, err := strconv.Atoi(matches[1])
	if err != nil {
		return
	}

	setRuntimeRunStatus(job.key, fmt.Sprintf("exited (%d)", code))
}
