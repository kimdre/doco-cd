package docker

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"
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

		ret.DeployedServicesName = append(ret.DeployedServicesName, string(svcName))

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
