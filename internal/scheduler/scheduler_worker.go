package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/graceful"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/prometheus"
)

const (
	schedulerEventReconnectDelay = time.Second
	schedulerRefreshRetryDelay   = time.Second
)

func (s *scheduler) run(ctx context.Context) {
	defer s.runs.Wait()

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

		cfg, enabled, parseErr := parseJobConfig(job)
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
				// Keep last run across fingerprint changes (schedule/label edits).
				lastRun: prevState.lastRun,
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

	s.runtime.setStatesSnapshot(s.contextName, s.mode, s.states)

	return nearestNextRun, !nearestNextRun.IsZero()
}

func (s *scheduler) watchJobChanges(ctx context.Context) <-chan struct{} {
	changes := make(chan struct{}, 1)

	graceful.SafeGo(s.wg, s.log, func() {
		defer close(changes)

		for ctx.Err() == nil {
			filters := make(client.Filters)
			if s.mode == scheduledJobModeSwarm {
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
	s.runs.Add(1)

	graceful.SafeGo(s.wg, s.log, func() {
		defer s.runs.Done()
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

		runID := id.New()

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
		unlockStacks := lockStacks(s.contextName, lockedStacks...)

		defer unlockStacks()

		runLog.Debug("acquired scheduler/deploy lock(s)")

		runLog.Debug("triggering scheduled run")

		err := s.executeScheduledRun(ctx, job, cfg)
		s.runtime.updateRunStatus(job, cfg, err)

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
