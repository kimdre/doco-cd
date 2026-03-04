package swarm

import (
	"context"

	"github.com/docker/cli/cli/compose/convert"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

const (
	StackNamespaceLabel = "com.docker.stack.namespace"
)

var ModeEnabled bool // Whether the docker host is running in swarm mode

func getStackFilter(namespace string) client.Filters {
	return make(client.Filters).Add("label", convert.LabelNamespace+"="+namespace)
}

func GetStackServices(ctx context.Context, apiclient client.APIClient, namespace string) ([]swarm.Service, error) {
	result, err := apiclient.ServiceList(ctx, client.ServiceListOptions{Filters: getStackFilter(namespace)})
	if err != nil {
		return nil, err
	}

	return result.Items, nil
}

func GetServicesByLabel(ctx context.Context, apiclient client.APIClient, labelKey, labelValue string) ([]swarm.Service, error) {
	filter := make(client.Filters).Add("label", labelKey+"="+labelValue)

	result, err := apiclient.ServiceList(ctx, client.ServiceListOptions{Filters: filter})
	if err != nil {
		return nil, err
	}

	return result.Items, nil
}

func getStackNetworks(ctx context.Context, apiclient client.APIClient, namespace string) ([]network.Summary, error) {
	result, err := apiclient.NetworkList(ctx, client.NetworkListOptions{Filters: getStackFilter(namespace)})
	if err != nil {
		return nil, err
	}

	return result.Items, nil
}

func getStackSecrets(ctx context.Context, apiclient client.APIClient, namespace string) ([]swarm.Secret, error) {
	result, err := apiclient.SecretList(ctx, client.SecretListOptions{Filters: getStackFilter(namespace)})
	if err != nil {
		return nil, err
	}

	return result.Items, nil
}

func getStackConfigs(ctx context.Context, apiclient client.APIClient, namespace string) ([]swarm.Config, error) {
	result, err := apiclient.ConfigList(ctx, client.ConfigListOptions{Filters: getStackFilter(namespace)})
	if err != nil {
		return nil, err
	}

	return result.Items, nil
}

func getStackTasks(ctx context.Context, apiclient client.APIClient, namespace string) ([]swarm.Task, error) {
	result, err := apiclient.TaskList(ctx, client.TaskListOptions{Filters: getStackFilter(namespace)})
	if err != nil {
		return nil, err
	}

	return result.Items, nil
}

func GetStacks(ctx context.Context, apiclient client.APIClient) (map[string][]swarm.Service, error) {
	stacks := make(map[string][]swarm.Service)

	result, err := apiclient.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		return nil, err
	}

	for _, service := range result.Items {
		if namespace, ok := service.Spec.Labels[StackNamespaceLabel]; ok {
			stacks[namespace] = append(stacks[namespace], service)
		}
	}

	return stacks, nil
}
