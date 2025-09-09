package swarm

import (
	"context"

	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
)

const (
	StackNamespaceLabel = "com.docker.stack.namespace"
)

var ModeEnabled bool // Whether the docker host is running in swarm mode

func getStackFilter(namespace string) filters.Args {
	filter := filters.NewArgs()
	filter.Add("label", convert.LabelNamespace+"="+namespace)

	return filter
}

func getStackServices(ctx context.Context, apiclient client.APIClient, namespace string) ([]swarm.Service, error) {
	return apiclient.ServiceList(ctx, swarm.ServiceListOptions{Filters: getStackFilter(namespace)})
}

func getStackNetworks(ctx context.Context, apiclient client.APIClient, namespace string) ([]network.Summary, error) {
	return apiclient.NetworkList(ctx, network.ListOptions{Filters: getStackFilter(namespace)})
}

func getStackSecrets(ctx context.Context, apiclient client.APIClient, namespace string) ([]swarm.Secret, error) {
	return apiclient.SecretList(ctx, swarm.SecretListOptions{Filters: getStackFilter(namespace)})
}

func getStackConfigs(ctx context.Context, apiclient client.APIClient, namespace string) ([]swarm.Config, error) {
	return apiclient.ConfigList(ctx, swarm.ConfigListOptions{Filters: getStackFilter(namespace)})
}

func getStackTasks(ctx context.Context, apiclient client.APIClient, namespace string) ([]swarm.Task, error) {
	return apiclient.TaskList(ctx, swarm.TaskListOptions{Filters: getStackFilter(namespace)})
}
