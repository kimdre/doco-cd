package docker

import (
	"context"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

type runtimeQueryTestClient struct {
	client.APIClient

	containers       []container.Summary
	services         []swarm.Service
	containerState   *container.State
	containerOptions client.ContainerListOptions
	serviceOptions   client.ServiceListOptions
	inspectedID      string
}

func (c *runtimeQueryTestClient) ContainerList(_ context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	c.containerOptions = options
	return client.ContainerListResult{Items: c.containers}, nil
}

func (c *runtimeQueryTestClient) ServiceList(_ context.Context, options client.ServiceListOptions) (client.ServiceListResult, error) {
	c.serviceOptions = options
	return client.ServiceListResult{Items: c.services}, nil
}

func (c *runtimeQueryTestClient) ContainerInspect(_ context.Context, containerID string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	c.inspectedID = containerID

	return client.ContainerInspectResult{
		Container: container.InspectResponse{State: c.containerState},
	}, nil
}

func TestListManagedRepositoryContainers(t *testing.T) {
	t.Parallel()

	apiClient := &runtimeQueryTestClient{
		containers: []container.Summary{{ID: "container-id"}},
	}

	got, err := ListManagedRepositoryContainers(t.Context(), apiClient, "owner/repository", true)
	if err != nil {
		t.Fatal(err)
	}

	wantFilters := make(client.Filters)
	wantFilters.Add("label", DocoCDLabels.Metadata.Manager+"=doco-cd")
	wantFilters.Add("label", DocoCDLabels.Source.Name+"=owner/repository")

	if !reflect.DeepEqual(got, apiClient.containers) {
		t.Fatalf("containers = %#v, want %#v", got, apiClient.containers)
	}

	if !apiClient.containerOptions.All || !reflect.DeepEqual(apiClient.containerOptions.Filters, wantFilters) {
		t.Fatalf("container options = %#v, want All=true and filters %#v", apiClient.containerOptions, wantFilters)
	}
}

func TestListManagedRepositoryServices(t *testing.T) {
	t.Parallel()

	apiClient := &runtimeQueryTestClient{
		services: []swarm.Service{{ID: "service-id"}},
	}

	got, err := ListManagedRepositoryServices(t.Context(), apiClient, "owner/repository")
	if err != nil {
		t.Fatal(err)
	}

	wantFilters := make(client.Filters)
	wantFilters.Add("label", DocoCDLabels.Metadata.Manager+"=doco-cd")
	wantFilters.Add("label", DocoCDLabels.Source.Name+"=owner/repository")

	if !reflect.DeepEqual(got, apiClient.services) {
		t.Fatalf("services = %#v, want %#v", got, apiClient.services)
	}

	if !reflect.DeepEqual(apiClient.serviceOptions.Filters, wantFilters) {
		t.Fatalf("service filters = %#v, want %#v", apiClient.serviceOptions.Filters, wantFilters)
	}
}

func TestInspectContainerState(t *testing.T) {
	t.Parallel()

	want := &container.State{Status: "running"}
	apiClient := &runtimeQueryTestClient{containerState: want}

	got, err := InspectContainerState(t.Context(), apiClient, "container-id")
	if err != nil {
		t.Fatal(err)
	}

	if got != want || apiClient.inspectedID != "container-id" {
		t.Fatalf("state = %#v, inspected ID = %q", got, apiClient.inspectedID)
	}
}
