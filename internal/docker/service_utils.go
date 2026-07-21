package docker

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	swarmInternal "github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/utils/set"
)

type ServiceStatus struct {
	// In non-Swarm mode:
	// Labels may differ between containers within a service, but most of them should be identical for the same service,
	// except for com.docker.compose.container-number, com.docker.compose.replace, and potentially others.
	Labels Labels

	// swarm deploy mode.
	// Empty if not in swarm mode.
	SwarmMode swarmInternal.DeployMode

	// Non-swarm mode: number of running containers
	// Swarm mode: number of service replicas
	Replicas uint64
}

type LatestServiceStatus struct {
	deploymentCommitSHA   string
	deploymentComposeHash string

	DeployedStatus map[Service]ServiceStatus
}

func (l LatestServiceStatus) GetDeploymentCommitSHA() string {
	return l.deploymentCommitSHA
}

func (l LatestServiceStatus) GetDeploymentComposeHash() string {
	return l.deploymentComposeHash
}

// normalizeRepositoryForLabelMatch normalizes repository strings for label matching by trimming whitespace,
// converting to host/owner/repo format when possible, and stripping OCI digest and tag suffixes.
func normalizeRepositoryForLabelMatch(repository string) string {
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return ""
	}

	// Normalize scheme/scp-like urls to host/owner/repo when possible.
	repository = git.GetRepoName(repository)

	// Strip OCI digest suffix if present (repo@sha256:...).
	if idx := strings.LastIndex(repository, "@"); idx > 0 {
		repository = repository[:idx]
	}

	// Strip OCI tag suffix from the last path segment (repo:tag).
	parts := strings.Split(repository, "/")
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		if idx := strings.LastIndex(last, ":"); idx > 0 {
			parts[len(parts)-1] = last[:idx]
		}

		repository = strings.Join(parts, "/")
	}

	return strings.TrimSpace(repository)
}

// buildRepositoryLabelCandidates generates a set of candidate repository label values
// for matching by normalizing the input repository string.
func buildRepositoryLabelCandidates(repository string) map[string]struct{} {
	if strings.TrimSpace(repository) == "" {
		return map[string]struct{}{"": {}}
	}

	candidates := map[string]struct{}{}

	add := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" {
			candidates[v] = struct{}{}
		}
	}

	normalized := normalizeRepositoryForLabelMatch(repository)
	add(normalized)
	add(git.GetFullName(normalized))

	return candidates
}

// GetLatestDeployStatus retrieves the deployed status for a given repository and deploy name.
func GetLatestDeployStatus(ctx context.Context, client client.APIClient, swarmMode bool, cloneURL string, deployName string) (LatestServiceStatus, error) {
	serviceLabels, err := getDeployStatus(ctx, client, swarmMode, deployName)
	if err != nil {
		return LatestServiceStatus{}, fmt.Errorf("failed to retrieve service labels: %w", err)
	}

	return getLatestServiceStatus(&deployStatusCache, serviceLabels, cloneURL, deployName), nil
}

func getLatestServiceStatus(cacheMap *sync.Map, statusMap map[Service]ServiceStatus, repository string, deployName string) LatestServiceStatus {
	ret := LatestServiceStatus{
		DeployedStatus: make(map[Service]ServiceStatus),
	}

	repositoryLabelCandidates := buildRepositoryLabelCandidates(repository)

	var (
		latestLabels    Labels
		latestTimestamp string
	)

	for serviceName, state := range statusMap {
		// Always include the service in the deployed inventory, regardless of whether it has
		// cd.doco.* labels. Containers that are missing those labels (e.g. started via the
		// Docker CLI directly or deployed before doco-cd stamped them) are still genuinely
		// running and must not be reported as "service not deployed" by CheckServiceMismatch.
		ret.DeployedStatus[serviceName] = state

		// Only use containers that carry doco-cd repository labels for deployment metadata
		// (latest commit SHA, compose hash). This keeps the two concerns separate.
		labels := state.Labels

		name, ok := labels[DocoCDLabels.Source.Name]
		if !ok {
			// When a service matches and others don't,
			// using 'break' will return a random result.
			continue
		}

		if _, matches := repositoryLabelCandidates[strings.TrimSpace(name)]; !matches {
			// When a service matches and others don't,
			// using 'break' will return a random result.
			continue
		}

		timestamp := labels[DocoCDLabels.Deployment.Timestamp]
		// Get the candidate with the latest timestamp for the most recent deployment comparison.
		// Use 'equal' here; ensure latestLabels is not empty if timestamp is empty.
		// TODO: If timestamps are equal, the result may be random for simultaneous deployments.
		if timestamp >= latestTimestamp {
			latestTimestamp = timestamp
			latestLabels = labels
		}
	}

	cache, ok := getDeployStatusFromCache(cacheMap, normalizeRepositoryForLabelMatch(repository), deployName)
	if ok {
		ret.deploymentCommitSHA = cache.CommitSHA
		ret.deploymentComposeHash = cache.ComposeHash
	} else {
		ret.deploymentCommitSHA, _ = latestLabels.getDeploymentCommitSHA()
		ret.deploymentComposeHash, _ = latestLabels.getDeploymentComposeHash()
	}

	return ret
}

func getDeployStatus(ctx context.Context, client client.APIClient, swarmMode bool, deployName string) (map[Service]ServiceStatus, error) {
	result := make(map[Service]ServiceStatus)

	if swarmMode {
		services, err := swarmInternal.GetStackServices(ctx, client, deployName)
		if err != nil {
			return nil, fmt.Errorf("failed to get services for stack %s: %w", deployName, err)
		}

		ns := convert.NewNamespace(deployName)

		for _, service := range services {
			status := ServiceStatus{}
			if service.Spec.TaskTemplate.ContainerSpec != nil {
				status.Labels = service.Spec.TaskTemplate.ContainerSpec.Labels
			}

			mode := service.Spec.Mode
			switch {
			case mode.Replicated != nil:
				status.SwarmMode = swarmInternal.DeployModeReplicated
				status.Replicas = *mode.Replicated.Replicas
			case mode.Global != nil:
				status.SwarmMode = swarmInternal.DeployModeGlobal
			case mode.ReplicatedJob != nil:
				status.SwarmMode = swarmInternal.DeployModeReplicatedJob
				status.Replicas = *mode.ReplicatedJob.TotalCompletions
			case mode.GlobalJob != nil:
				status.SwarmMode = swarmInternal.DeployModeGlobalJob
			}

			name := ns.Descope(service.Spec.Name)
			result[Service(name)] = status
		}
	} else {
		containers, err := GetLabeledContainers(ctx, client, api.ProjectLabel, deployName, true)
		if err != nil {
			return nil, fmt.Errorf("failed to get containers for project %s: %w", deployName, err)
		}

		result = getServiceStatusFromContainerStatus(deployName, containers)
	}

	return result, nil
}

// getServiceStatusFromContainerStatus returns a map of service names to their current status.
func getServiceStatusFromContainerStatus(projectName string, containers []container.Summary) map[Service]ServiceStatus {
	result := make(map[Service]ServiceStatus)

	for _, cont := range containers {
		if cont.Labels[api.ProjectLabel] != projectName {
			continue
		}

		serviceName := cont.Labels[api.ServiceLabel]

		status, ok := result[Service(serviceName)]
		if !ok {
			// the labels may be different between containers, but they should be the same for the same service,
			// except com.docker.compose.container-number, com.docker.compose.replace
			// so just use the labels of the first container we encounter for each service
			status = ServiceStatus{}
			status.Labels = cont.Labels
		}

		if cont.State == container.StateRunning {
			status.Replicas++
		}

		// Keep service presence even when all containers are stopped.
		result[Service(serviceName)] = status
	}

	return result
}

const (
	ServiceMismatchReasonNotDeployed = "service not deployed"
	ServiceMismatchReasonUnnecessary = "service is unnecessary"
	ServiceMismatchReasonSwarmMode   = "swarm mode mismatch"
	ServiceMismatchReasonReplicas    = "replicas mismatch"
	oneOffServiceNameSeparator       = "-doco-job-"
	legacyOneShotExecutionMode       = "one_shot"
)

type ServiceMismatch struct {
	ServiceName string                  `json:"service_name"`
	Reasons     []ServiceMismatchReason `json:"reasons"`
}

type ServiceMismatchReason struct {
	Reason string `json:"reason"`
	Want   any    `json:"want"`
	Got    any    `json:"got"`
}

// CheckServiceMismatch checks if the deployed services match the services in the compose file.
// now only check replicas, swarm mode, missing and unnecessary services.
func CheckServiceMismatch(swarmModeEnabled bool, deployed map[Service]ServiceStatus, services types.Services) []ServiceMismatch {
	var mismatches []ServiceMismatch

	// In non-Swarm mode, services with unset, "on-failure", or "no" restart policy can be considered as "stopped" and won't cause a mismatch.
	allowStoppedForRestartPolicy := func(svc types.ServiceConfig) bool {
		restart := strings.ToLower(strings.TrimSpace(svc.Restart))
		return restart == "" || strings.HasPrefix(restart, "on-failure") || restart == "no"
	}

	getSvcMode := func(svc types.ServiceConfig) swarmInternal.DeployMode {
		if !swarmModeEnabled {
			return ""
		}

		if svc.Deploy == nil || svc.Deploy.Mode == "" {
			return swarmInternal.DeployModeReplicated
		}

		return swarmInternal.DeployMode(svc.Deploy.Mode)
	}
	for svcName, svc := range services {
		status, ok := deployed[Service(svcName)]

		reasons := []ServiceMismatchReason{}

		if swarmModeEnabled {
			if !ok {
				reasons = append(reasons, ServiceMismatchReason{
					Reason: ServiceMismatchReasonNotDeployed,
				})
			} else {
				svcMode := getSvcMode(svc)
				if status.SwarmMode != svcMode {
					reasons = append(reasons, ServiceMismatchReason{
						Reason: ServiceMismatchReasonSwarmMode,
						Want:   svcMode,
						Got:    status.SwarmMode,
					})
				} else {
					switch svcMode {
					case swarmInternal.DeployModeReplicated, swarmInternal.DeployModeReplicatedJob:
						//  scale should always be >= 0
						if uint64(svc.GetScale()) != status.Replicas { //nolint:gosec
							reasons = append(reasons, ServiceMismatchReason{
								Reason: ServiceMismatchReasonReplicas,
								Want:   svc.GetScale(),
								Got:    status.Replicas,
							})
						}
					}
				}
			}
		} else if !allowStoppedForRestartPolicy(svc) {
			if !ok {
				reasons = append(reasons, ServiceMismatchReason{
					Reason: ServiceMismatchReasonNotDeployed,
				})
			} else if uint64(svc.GetScale()) != status.Replicas { //nolint:gosec
				reasons = append(reasons, ServiceMismatchReason{
					Reason: ServiceMismatchReasonReplicas,
					Want:   svc.GetScale(),
					Got:    status.Replicas,
				})
			}
		}

		if len(reasons) > 0 {
			mismatches = append(mismatches, ServiceMismatch{
				ServiceName: svcName,
				Reasons:     reasons,
			})
		}
	}

	deployedSet := set.New[string]()
	for name := range maps.Keys(deployed) {
		deployedSet.Add(string(name))
	}

	wantSet := set.New[string]()
	for name := range maps.Keys(services) {
		wantSet.Add(name)
	}

	for name := range deployedSet.Difference(wantSet) {
		if isEphemeralOneOffService(name, deployed[Service(name)], wantSet) {
			continue
		}

		mismatches = append(mismatches, ServiceMismatch{
			ServiceName: name,
			Reasons: []ServiceMismatchReason{
				{
					Reason: ServiceMismatchReasonUnnecessary,
				},
			},
		})
	}

	return mismatches
}

func isEphemeralOneOffService(name string, status ServiceStatus, declaredServices set.Set[string]) bool {
	labels := status.Labels
	if labels == nil {
		labels = Labels{}
	}

	if raw, ok := labels[DocoCDJobLabels.JobEphemeral]; ok {
		isEphemeral, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err == nil {
			return isEphemeral
		}
	}

	mode := strings.TrimSpace(labels[DocoCDJobLabels.JobExecutionMode])
	if mode != string(JobExecutionModeOneOff) && mode != legacyOneShotExecutionMode {
		return false
	}

	idx := strings.LastIndex(name, oneOffServiceNameSeparator)
	if idx <= 0 || idx+len(oneOffServiceNameSeparator) >= len(name) {
		return false
	}

	baseName := strings.TrimSpace(name[:idx])

	suffix := strings.TrimSpace(name[idx+len(oneOffServiceNameSeparator):])
	if baseName == "" || suffix == "" {
		return false
	}

	if _, err := strconv.ParseInt(suffix, 10, 64); err != nil {
		return false
	}

	return declaredServices.Contains(baseName)
}
