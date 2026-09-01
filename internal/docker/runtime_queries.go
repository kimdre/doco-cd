package docker

import (
	"context"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
)

// ListManagedRepositoryContainers lists all containers that belong to a specific repository.
func ListManagedRepositoryContainers(ctx context.Context, apiClient client.APIClient, repository string, all bool) ([]container.Summary, error) {
	filters := make(client.Filters)
	filters.Add("label", DocoCDLabels.Metadata.Manager+"="+app.Name)
	filters.Add("label", DocoCDLabels.Source.Name+"="+repository)

	result, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
		All:     all,
		Filters: filters,
	})
	if err != nil {
		return nil, err
	}

	return result.Items, nil
}

// InspectContainerState inspects the state of a specific container.
func InspectContainerState(ctx context.Context, apiClient client.APIClient, containerID string) (*container.State, error) {
	result, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}

	return result.Container.State, nil
}

// ListManagedRepositoryServices lists all services that belong to a specific repository.
func ListManagedRepositoryServices(ctx context.Context, apiClient client.APIClient, repository string) ([]swarm.Service, error) {
	filters := make(client.Filters)
	filters.Add("label", DocoCDLabels.Metadata.Manager+"="+app.Name)
	filters.Add("label", DocoCDLabels.Source.Name+"="+repository)

	result, err := apiClient.ServiceList(ctx, client.ServiceListOptions{Filters: filters})
	if err != nil {
		return nil, err
	}

	return result.Items, nil
}
