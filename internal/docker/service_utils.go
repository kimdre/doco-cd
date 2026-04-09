package docker

import (
	"context"
	"fmt"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	swarmInternal "github.com/kimdre/doco-cd/internal/docker/swarm"
)

type ServiceStatus struct {
	// in no swarm mode
	// the labels may be different between containers in a service, but most of they should be the same for the same service,
	// except com.docker.compose.container-number, com.docker.compose.replace, maybe more.
	//
	// in swarm mode, the labels of tasks are the same as the service, so just use the labels of service
	Labels Labels

	// swarm deploy mode
	// empty if not in swarm mode
	SwarmMode string

	// in no swarm mode, number is running container count
	// in swarm mode, number is the service's replicas
	Replicas uint64
}

const (
	swarmModeReplicated    = "replicated"
	swarmModeReplicatedJob = "replicated-job"
	swarmModeGlobal        = "global"
	swarmModeGlobalJob     = "global-job"
)

type LatestServiceStatus struct {
	// the labels may be different between in different service, but project level labels should be the same.
	Labels Labels

	DeployedStatus map[Service]ServiceStatus
}

// GetLatestDeployStatus retrieves the labels of the most recently (re-)deployed services for a given repository and deploy name.
func GetLatestDeployStatus(ctx context.Context, client *client.Client, repoName, deployName string) (LatestServiceStatus, error) {
	serviceLabels, err := getDeployStatus(ctx, client, deployName)
	if err != nil {
		return LatestServiceStatus{}, fmt.Errorf("failed to retrieve service labels: %w", err)
	}

	return getLatestServiceStatus(serviceLabels, repoName), nil
}

// getLatestServiceStatus returns the labels of the most recently (re-)deployed services for a given repository.
func getLatestServiceStatus(statusMap map[Service]ServiceStatus, repoName string) LatestServiceStatus {
	ret := LatestServiceStatus{
		DeployedStatus: make(map[Service]ServiceStatus),
		Labels:         make(Labels),
	}

	var latestTimestamp string

	for serviceName, state := range statusMap {
		labels := state.Labels

		name, ok := labels[DocoCDLabels.Repository.Name]
		if !ok || name != repoName {
			// when service matches and some others don't,
			// if use break random result will be returned.
			continue
		}

		timestamp := labels[DocoCDLabels.Deployment.Timestamp]
		// Get the candidate with the latest timestamp to ensure we are comparing against the most recent deployment.
		// use equal here, make latestLabels not empty when timestamp is empty
		// todo: when timestamp is equal, result may random when deployed at the same timestamp multiple times
		if timestamp >= latestTimestamp {
			latestTimestamp = timestamp
			ret.Labels = labels
		}

		ret.DeployedStatus[serviceName] = state
	}

	return ret
}

func getDeployStatus(ctx context.Context, client *client.Client, deployName string) (map[Service]ServiceStatus, error) {
	result := make(map[Service]ServiceStatus)

	if swarmInternal.ModeEnabled {
		services, err := swarmInternal.GetStackServices(ctx, client, deployName)
		if err != nil {
			return nil, fmt.Errorf("failed to get services for stack %s: %w", deployName, err)
		}

		ns := convert.NewNamespace(deployName)

		for _, service := range services {
			status := ServiceStatus{
				Labels: service.Spec.TaskTemplate.ContainerSpec.Labels,
			}

			mode := service.Spec.Mode
			switch {
			case mode.Replicated != nil:
				status.SwarmMode = swarmModeReplicated
				status.Replicas = *mode.Replicated.Replicas
			case mode.Global != nil:
				status.SwarmMode = swarmModeGlobal
			case mode.ReplicatedJob != nil:
				status.SwarmMode = swarmModeReplicatedJob
				status.Replicas = *mode.ReplicatedJob.TotalCompletions
			case mode.GlobalJob != nil:
				status.SwarmMode = swarmModeGlobalJob
			}

			name := ns.Descope(service.Spec.Name)
			result[Service(name)] = status
		}
	} else {
		containers, err := GetLabeledContainers(ctx, client, api.ProjectLabel, deployName)
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
		if cont.State == container.StateRunning && cont.Labels[api.ProjectLabel] == projectName {
			serviceName := cont.Labels[api.ServiceLabel]

			status, ok := result[Service(serviceName)]
			if !ok {
				// the labels may be different between containers, but they should be the same for the same service,
				// except com.docker.compose.container-number, com.docker.compose.replace
				// so just use the labels of the first container we encounter for each service
				status = ServiceStatus{}
				status.Labels = cont.Labels
			}

			status.Replicas++

			result[Service(serviceName)] = status
		}
	}

	return result
}

const (
	ServiceMismatchReasonNotDeployed = "service not deployed"
	ServiceMismatchReasonSwarmMode   = "swarm mode mismatch"
	ServiceMismatchReasonReplicas    = "replicas mismatch"
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

func CheckServiceMismatch(swarmModeEnabled bool, deployed map[Service]ServiceStatus, services types.Services) []ServiceMismatch {
	var mismatches []ServiceMismatch

	for svcName, svc := range services {
		status, ok := deployed[Service(svcName)]

		reasons := []ServiceMismatchReason{}
		if !ok {
			reasons = append(reasons, ServiceMismatchReason{
				Reason: ServiceMismatchReasonNotDeployed,
			})
		} else {
			if swarmModeEnabled {
				if status.SwarmMode != svc.Deploy.Mode {
					reasons = append(reasons, ServiceMismatchReason{
						Reason: ServiceMismatchReasonSwarmMode,
						Want:   svc.Deploy.Mode,
						Got:    status.SwarmMode,
					})
				} else {
					switch svc.Deploy.Mode {
					case swarmModeReplicated, swarmModeReplicatedJob:
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
			} else {
				if uint64(svc.GetScale()) != status.Replicas { //nolint:gosec
					reasons = append(reasons, ServiceMismatchReason{
						Reason: ServiceMismatchReasonReplicas,
						Want:   svc.GetScale(),
						Got:    status.Replicas,
					})
				}
			}
		}

		if len(reasons) > 0 {
			mismatches = append(mismatches, ServiceMismatch{
				ServiceName: svcName,
				Reasons:     reasons,
			})
		}
	}

	return mismatches
}
