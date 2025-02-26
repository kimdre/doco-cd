package docker

import (
	"context"
	"errors"
	"strings"

	"github.com/docker/docker/api/types/container"
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