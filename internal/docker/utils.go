package docker

import (
	"context"
	"errors"
	"strings"

	"github.com/docker/docker/api/types"
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
		for _, contname := range container.Names {
			if strings.Contains(contname, name) { // Match by service name
				return container.ID, nil
			}
		}
	}

	return "", errors.New("container id not found")
}

func GetLabeledContainers(ctx context.Context, cli *client.Client, key, value string) (containers []types.Container, err error) {
	filters := filters.NewArgs(filters.Arg("label", key+"="+value))

	containers, err = cli.ContainerList(ctx, container.ListOptions{
		Filters: filters,
		All:     false,
	})
	if err != nil {
		return nil, err
	}

	return containers, nil
}