package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
)

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
			err := docker.RunComposeOneOffFromServiceDefinition(ctx, s.dockerCli, job.labels, s.secretProvider)
			if err == nil {
				return nil
			}

			if !errors.Is(err, docker.ErrComposeScheduledMetadataUnavailable) {
				return err
			}

			return docker.RunContainerOneOffFromExisting(ctx, s.dockerCli.Client(), job.id)
		default:
			err := docker.RunComposeScheduledContainer(ctx, s.dockerCli, job.id, job.labels, len(cfg.StopServices) > 0, s.secretProvider)
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
	key := stopHoldKey{context: s.contextName, mode: mode, project: project, service: service}

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
	key := stopHoldKey{context: s.contextName, mode: mode, project: project, service: service}

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
	key := stopHoldKey{context: s.contextName, mode: mode, project: project, service: service}

	s.stopHoldsMu.Lock()
	defer s.stopHoldsMu.Unlock()

	st, ok := s.stopHolds[key]
	if !ok {
		// No recorded hold (shouldn't normally happen), so there is nothing to restore.
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
// records an additional hold on it but does not stop it again. See
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
				if s.stopHoldTracker != nil {
					s.stopHoldTracker.MarkSchedulerStopHeld(s.contextName, project, svc)
				}
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
// released their hold (see releaseStopHold). If another concurrent run is
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

			// Unmark the scheduler hold before starting. If our start fails,
			// reconciliation can step in and recover the service.
			for _, svc := range toStart {
				if s.stopHoldTracker != nil {
					s.stopHoldTracker.UnmarkSchedulerStopHeld(s.contextName, project, svc)
				}
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
				// actually stopped by us in the first place, so there is nothing to restore.
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
// order. Locking multiple stacks in a fixed global order rather than in
// caller-supplied order prevents ABBA deadlocks when two scheduled runs
// need to lock an overlapping set of stacks concurrently (e.g. a job's own
// stack plus stacks referenced by its stop_services, which another run might
// need to lock in the opposite order). Stack names are only unique within a
// Docker context (see lock.StackKey), so contextName is included in every
// lock key to keep same-named stacks on different contexts from blocking each
// other. It returns an unlock function that releases all acquired locks;
// callers must call it exactly once.
func lockStacks(contextName string, stacks ...string) (unlock func()) {
	seen := make(map[string]struct{}, len(stacks))
	unique := make([]string, 0, len(stacks))

	for _, stack := range stacks {
		stack = strings.TrimSpace(stack)
		if stack == "" {
			continue
		}

		key := lock.StackKey(contextName, stack)
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}
		unique = append(unique, key)
	}

	sort.Strings(unique)

	for _, key := range unique {
		lock.LockStack(key)
	}

	return func() {
		for _, u := range slices.Backward(unique) {
			lock.UnlockStack(u)
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
		Context:           docker.DisplayContextName(job.context),
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
	return s.runtime.isRunInProgress(key)
}

func (s *scheduler) setRunInProgress(key string, inProgress bool) {
	if inProgress {
		s.runtime.beginRun(s.contextName, s.mode, key)
		return
	}

	s.runtime.endRun(key)
}

func getScheduledRunMetricLabels(job scheduledJob, cfg docker.JobScheduleConfig, stackName string) []string {
	return []string{docker.DisplayContextName(job.context), stackName, job.name, string(job.mode), string(cfg.ExecutionMode)}
}
