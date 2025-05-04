package docker

import (
	"context"
	"errors"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

func GetContainerID(client client.APIClient, name string) (id string, err error) {
	containers, err := client.ContainerList(context.TODO(), container.ListOptions{All: true})
	if err != nil {
		return "", err
	}

	for _, container := range containers {
		for _, containerName := range container.Names {
			if strings.Contains(containerName, name) { // Match by service name
				return container.ID, nil
			}
		}
	}

	return "", errors.New("container id not found")
}

func GetLabeledContainers(ctx context.Context, cli *client.Client, key, value string) (containers []container.Summary, err error) {
	containers, err = cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", key+"="+value)),
		All:     false,
	})
	if err != nil {
		return nil, err
	}

	return containers, nil
}
