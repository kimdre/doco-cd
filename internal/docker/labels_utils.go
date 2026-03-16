package docker

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"
)

type candidate struct {
	timestamp string
	labels    Labels
}

// GetLatestServiceLabels retrieves the labels of the most recently (re-)deployed services for a given repository and deployment name..
func GetLatestServiceLabels(ctx context.Context, client *client.Client, repoName, deployName string) (Labels, error) {
	var (
		candidates   []candidate
		latestLabels Labels
	)

	serviceLabels, err := GetServiceLabels(ctx, client, deployName)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve service labels: %w", err)
	}

	// Find deployed commit, deployConfig hash and externalSecrets hash from labels of deployed services
	for _, labels := range serviceLabels {
		name, ok := labels[DocoCDLabels.Repository.Name]
		if !ok || name != repoName {
			break
		}

		candidates = append(candidates, candidate{
			timestamp: labels[DocoCDLabels.Deployment.Timestamp],
			labels:    labels,
		})
	}

	// Get the candidate with the latest timestamp to ensure we are comparing against the most recent deployment
	if len(candidates) > 0 {
		latestCandidate := candidates[0]

		for _, c := range candidates[1:] {
			if c.timestamp > latestCandidate.timestamp {
				latestCandidate = c
			}
		}

		latestLabels = latestCandidate.labels
	}

	return latestLabels, nil
}
