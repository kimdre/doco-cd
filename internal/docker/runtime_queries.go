package docker

import (
	"context"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

func ListManagedRepositoryContainers(ctx context.Context, apiClient client.APIClient, manager, repository string, all bool) ([]container.Summary, error) {
	filters := make(client.Filters)
	filters.Add("label", DocoCDLabels.Metadata.Manager+"="+manager)
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

func InspectContainerState(ctx context.Context, apiClient client.APIClient, containerID string) (*container.State, error) {
	result, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}

	return result.Container.State, nil
}

func ListManagedRepositoryServices(ctx context.Context, apiClient client.APIClient, manager, repository string) ([]swarm.Service, error) {
	filters := make(client.Filters)
	filters.Add("label", DocoCDLabels.Metadata.Manager+"="+manager)
	filters.Add("label", DocoCDLabels.Source.Name+"="+repository)

	result, err := apiClient.ServiceList(ctx, client.ServiceListOptions{Filters: filters})
	if err != nil {
		return nil, err
	}

	return result.Items, nil
}
