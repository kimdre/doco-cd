package docker

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"
)

// GetLatestServiceLabels retrieves the labels of the most recently (re-)deployed services for a given repository and deployment name..
func GetLatestServiceLabels(ctx context.Context, client *client.Client, repoName, deployName string) (Labels, error) {
	serviceLabels, err := GetServiceLabels(ctx, client, deployName)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve service labels: %w", err)
	}

	return getLatestServiceLabels(serviceLabels, repoName), nil
}

func getLatestServiceLabels(serviceLabels map[Service]Labels, repoName string) Labels {
	var (
		latestTimestamp string
		latestLabels    Labels
	)
	// Find deployed commit, deployConfig hash and externalSecrets hash from labels of deployed services
	for _, labels := range serviceLabels {
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
			latestLabels = labels
		}
	}

	return latestLabels
}
