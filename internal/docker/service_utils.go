package docker

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/utils/set"
)

type LatestServiceState struct {
	Labels Labels

	// DeployedServicesName is the names of the deployed services
	// for swarm mode, this is the service name
	// for no swarm mode, this is the container name
	DeployedServicesName []string
}

// GetLatestServiceState retrieves the labels of the most recently (re-)deployed services for a given repository and deployment name..
func GetLatestServiceState(ctx context.Context, client *client.Client, repoName, deployName string) (LatestServiceState, error) {
	serviceLabels, err := GetServiceLabels(ctx, client, deployName)
	if err != nil {
		return LatestServiceState{}, fmt.Errorf("failed to retrieve service labels: %w", err)
	}

	return getLatestServiceState(serviceLabels, repoName), nil
}

func getLatestServiceState(serviceLabels map[Service]Labels, repoName string) LatestServiceState {
	ret := LatestServiceState{}

	var latestTimestamp string
	// Find deployed commit, deployConfig hash and externalSecrets hash from labels of deployed services
	for svcName, labels := range serviceLabels {
		name, ok := labels[DocoCDLabels.Repository.Name]
		if !ok || name != repoName {
			// when service matches and some others don't,
			// if use break random result will be returned.
			continue
		}
		// container name start with / in no swarm mode
		svcName := strings.Trim(string(svcName), "/")
		ret.DeployedServicesName = append(ret.DeployedServicesName, svcName)

		timestamp := labels[DocoCDLabels.Deployment.Timestamp]
		// Get the candidate with the latest timestamp to ensure we are comparing against the most recent deployment.
		// use equal here, make latestLabels not empty when timestamp is empty
		// todo: when timestamp is equal, result may random when deployed at the same timestamp multiple times
		if timestamp >= latestTimestamp {
			latestTimestamp = timestamp
			ret.Labels = labels
		}
	}

	return ret
}

func CheckServiceMissing(deployed []string, projectName string, services types.Services) (missing []string) {
	if swarm.ModeEnabled {
		return getSwarmServiceMissing(deployed, projectName, services)
	}

	return getComposeServiceMissing(deployed, projectName, services)
}

func getSwarmServiceMissing(deployed []string, projectName string, services types.Services) (missing []string) {
	deployedSet := set.New(deployed...)

	ns := convert.NewNamespace(projectName)

	wantedSet := set.New[string]()
	for _, service := range services {
		wantedSet.Add(ns.Scope(service.Name))
	}

	return wantedSet.Difference(deployedSet).ToSlice()
}

func getComposeServiceMissing(deployed []string, projectName string, services types.Services) (missing []string) {
	// container_name maybe set in compose file, or generate by compose project-service-index
	deployedSet := set.New(deployed...)

	wantedSet := set.New[string]()

	for _, service := range services {
		if service.ContainerName != "" {
			wantedSet.Add(service.ContainerName)
		} else {
			replicas := service.GetScale()
			for i := 1; i <= replicas; i++ {
				name := getDefaultContainerName(projectName, service.Name, i)
				wantedSet.Add(name)
			}
		}
	}

	return wantedSet.Difference(deployedSet).ToSlice()
}

// modify from https://github.com/docker/compose/blob/main/pkg/compose/convergence.go#L413
func getDefaultContainerName(projectName string, serviceName string, number int) string {
	return strings.Join([]string{projectName, serviceName, strconv.Itoa(number)}, api.Separator)
}
