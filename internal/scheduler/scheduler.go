package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/kimdre/doco-cd/internal/utils/id"
)

const (
	schedulerEventReconnectDelay = time.Second
	schedulerRefreshRetryDelay   = time.Second
)

type scheduledJobMode string

const (
	scheduledJobModeContainer scheduledJobMode = "container"
	scheduledjobModeSwarm     scheduledJobMode = "swarm"
)

type scheduledJob struct {
	key    string
	name   string
	id     string
	mode   scheduledJobMode
	labels map[string]string
}

type scheduledJobState struct {
	fingerprint string
	schedule    cron.Schedule
	nextRun     time.Time
	cfg         docker.JobScheduleConfig
}

type scheduler struct {
	dockerCli command.Cli
	log       *slog.Logger
	wg        *sync.WaitGroup

	states map[string]scheduledJobState

	runningMu sync.Mutex
	running   map[string]bool
}

func Start(ctx context.Context, dockerCli command.Cli, log *slog.Logger, wg *sync.WaitGroup) {
	if dockerCli == nil || log == nil || wg == nil {
		return
	}

	s := &scheduler{
		dockerCli: dockerCli,
		log:       log.With(slog.String("component", "scheduler")),
		wg:        wg,
		states:    map[string]scheduledJobState{},
		running:   map[string]bool{},
	}

	s.run(ctx)
}

func (s *scheduler) run(ctx context.Context) {
	jobChanges := s.watchJobChanges(ctx)
	timer := time.NewTimer(time.Hour)

	stopTimer(timer)
	defer timer.Stop()

	s.log.Info("starting scheduler")

	nextRun, hasNextRun := s.refreshJobs(ctx, time.Now().UTC())

	for {
		setTimerToNextRun(timer, time.Now().UTC(), nextRun, hasNextRun)

		select {
		case <-ctx.Done():
			s.log.Info("scheduler stopped")
			return
		case _, ok := <-jobChanges:
			if !ok {
				jobChanges = nil
				continue
			}

			nextRun, hasNextRun = s.refreshJobs(ctx, time.Now().UTC())
		case t := <-timer.C:
			nextRun, hasNextRun = s.refreshJobs(ctx, t.UTC())
		}
	}
}

func (s *scheduler) refreshJobs(ctx context.Context, now time.Time) (time.Time, bool) {
	jobs, err := s.discoverJobs(ctx)
	if err != nil {
		s.log.Error("failed to discover scheduled jobs", logger.ErrAttr(err))
		return now.Add(schedulerRefreshRetryDelay), true
	}

	active := make(map[string]struct{}, len(jobs))

	var nearestNextRun time.Time

	for _, job := range jobs {
		cfg, enabled, parseErr := docker.ParseJobScheduleLabels(job.labels)
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

		state, ok := s.states[job.key]
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

		if !now.Before(state.nextRun) {
			scheduledAt := state.nextRun
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
			delete(s.states, key)
		}
	}

	if nearestNextRun.IsZero() {
		nearestNextRun, _ = getNearestNextRun(s.states)
	}

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
				mode:   scheduledjobModeSwarm,
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
			key:    key,
			name:   name,
			id:     c.ID,
			mode:   scheduledJobModeContainer,
			labels: c.Labels,
		}
	}

	result := make([]scheduledJob, 0, len(jobByKey))
	for _, job := range jobByKey {
		result = append(result, job)
	}

	return result, nil
}

func (s *scheduler) triggerRun(ctx context.Context, job scheduledJob, cfg docker.JobScheduleConfig, now time.Time) {
	if cfg.SkipRunning && s.isRunInProgress(job.key) {
		s.log.Warn("skipping scheduled run because previous run is still in progress",
			slog.String("job", job.name),
			slog.String("mode", string(job.mode)),
		)

		return
	}

	s.setRunInProgress(job.key, true)

	graceful.SafeGo(s.wg, s.log, func() {
		defer s.setRunInProgress(job.key, false)

		runID := id.GenID()
		stackName := getJobStackName(job)

		runLog := s.log.With(
			slog.String("job_id", runID),
			slog.String("job", job.name),
			slog.String("stack", stackName),
			slog.String("mode", string(job.mode)),
			slog.String("execution_mode", string(cfg.ExecutionMode)),
			slog.String("scheduled_at", now.Format(time.RFC3339)),
		)

		runLog.Debug("waiting for scheduler/deploy lock")
		lock.LockScheduledDeploy()

		defer lock.UnlockScheduledDeploy()

		runLog.Debug("acquired scheduler/deploy lock")

		runLog.Debug("triggering scheduled run")

		err := s.executeScheduledRun(ctx, job, cfg)
		if err != nil {
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
		case docker.JobExecutionModeOneShot:
			return docker.RunContainerOneShotFromExisting(ctx, s.dockerCli.Client(), job.id)
		default:
			return docker.RestartContainer(ctx, s.dockerCli.Client(), job.id)
		}
	case scheduledjobModeSwarm:
		switch cfg.ExecutionMode {
		case docker.JobExecutionModeOneShot:
			return docker.RunSwarmOneShotFromService(ctx, s.dockerCli, job.id, docker.SwarmOneShotFromServiceOptions{
				Replicas:         cfg.SwarmReplicas,
				SendRegistryAuth: true,
			})
		default:
			err := docker.RerunJobService(ctx, s.dockerCli.Client(), job.id)
			if err == nil {
				return nil
			}

			if errors.Is(err, docker.ErrNotAJobService) {
				return docker.RestartService(ctx, s.dockerCli.Client(), job.id)
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
	if job.mode == scheduledjobModeSwarm {
		actorKind = "service"
	}

	metadata := notification.Metadata{
		Repository:        job.labels[docker.DocoCDLabels.Repository.Name],
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
