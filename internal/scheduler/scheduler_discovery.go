package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

func (s *scheduler) discoverJobs(ctx context.Context) ([]scheduledJob, error) {
	if s.dockerCli == nil {
		return nil, nil
	}

	if s.mode == scheduledJobModeSwarm {
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
				key:     jobKeyPrefix(s.contextName) + "swarm:" + svc.ID,
				name:    svc.Spec.Name,
				id:      svc.Spec.Name,
				mode:    scheduledJobModeSwarm,
				labels:  labels,
				context: s.contextName,
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
		// A Swarm task is represented as a container too. On a Swarm manager it
		// is handled by the Swarm worker through its parent service, never by the
		// Compose worker.
		if _, isSwarmTask := c.Labels["com.docker.swarm.service.name"]; isSwarmTask {
			continue
		}

		key := jobKeyPrefix(s.contextName) + containerJobKey(c.ID, c.Labels)

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
			context:         s.contextName,
		}
	}

	result := make([]scheduledJob, 0, len(jobByKey))
	for key, job := range jobByKey {
		job.running = runningEphemeralByKey[key]
		result = append(result, job)
	}

	return result, nil
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
func parseJobConfig(job scheduledJob) (docker.JobScheduleConfig, bool, error) {
	cfg, enabled, err := docker.ParseJobScheduleLabels(job.labels)
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
	if ok {
		isEphemeral, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err == nil && isEphemeral {
			return true
		}
	}

	return strings.EqualFold(strings.TrimSpace(labels[api.OneoffLabel]), "true")
}

// jobKeyPrefix returns the prefix applied to every scheduler job/runtime state
// key discovered on contextName, so that the same compose project/service or
// swarm service ID on two different Docker daemons never collide in the
// (process-global) runtime state, run-status, or in-progress maps. The
// default context keeps the historical unprefixed keys for compatibility with
// state persisted before multi-context support existed.
func jobKeyPrefix(contextName string) string {
	if contextName == "" {
		return ""
	}

	return contextName + "::"
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
