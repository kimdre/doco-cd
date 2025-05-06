package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

var (
	ErrMountPointNotFound     = errors.New("mount point not found")
	ErrMountPointNotWriteable = errors.New("mount point is not writeable")
)

// GetContainerID retrieves the container ID for a given service name
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

// GetLabeledContainers retrieves all containers with a specific label key and value
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

// GetMountPointByDestination retrieves the mount point of a container volume/bind mount by its destination (mount point inside the container)
func GetMountPointByDestination(cli *client.Client, containerID, Destination string) (container.MountPoint, error) {
	// Get the container info
	cont, err := cli.ContainerInspect(context.TODO(), containerID)
	if err != nil {
		return container.MountPoint{}, fmt.Errorf("failed to inspect container %s: %w", containerID, err)
	}

	// Get the volume path
	for _, mount := range cont.Mounts {
		if mount.Destination == Destination {
			return mount, nil
		}
	}

	return container.MountPoint{}, fmt.Errorf("%w: %s", ErrMountPointNotFound, Destination)
}
