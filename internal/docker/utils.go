package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"

	"github.com/moby/moby/api/types/volume"

	swarmInternal "github.com/kimdre/doco-cd/internal/docker/swarm"

	"github.com/moby/moby/client"
)

var (
	ErrMountPointNotFound     = errors.New("mount point not found")
	ErrMountPointNotWriteable = errors.New("mount point is not writeable")
	ErrContainerIDNotFound    = errors.New("container ID not found")
)

// GetContainerID retrieves the container ID for a given service name.
func GetContainerID(apiClient client.APIClient, name string) (id string, err error) {
	result, err := apiClient.ContainerList(context.TODO(), client.ContainerListOptions{All: true})
	if err != nil {
		return "", err
	}

	for _, cont := range result.Items {
		for _, containerName := range cont.Names {
			if strings.Contains(containerName, name) { // Match by service name
				return cont.ID, nil
			}
		}
	}

	return "", fmt.Errorf("%w: %s", ErrContainerIDNotFound, name)
}

type (
	Service string            // Name of the Service
	Labels  map[string]string // Labels of the Service
)

// GetServiceLabels retrieves the Labels for each Service in a given stack.
func GetServiceLabels(ctx context.Context, cli *client.Client, stackName string) (map[Service]Labels, error) {
	if swarmInternal.ModeEnabled {
		services, err := swarmInternal.GetStackServices(ctx, cli, stackName)
		if err != nil {
			return nil, fmt.Errorf("failed to get services for stack %s: %w", stackName, err)
		}

		result := make(map[Service]Labels)
		for _, service := range services {
			result[Service(service.Spec.Name)] = service.Spec.TaskTemplate.ContainerSpec.Labels
		}

		return result, nil
	}

	containers, err := GetLabeledContainers(ctx, cli, api.ProjectLabel, stackName)
	if err != nil {
		return nil, fmt.Errorf("failed to get containers for stack %s: %w", stackName, err)
	}

	result := make(map[Service]Labels)
	for _, cont := range containers {
		result[Service(cont.Names[0])] = cont.Labels
	}

	return result, nil
}

// GetLabeledContainers retrieves all containers with a specific label key and value.
func GetLabeledContainers(ctx context.Context, cli *client.Client, key, value string) (containers []container.Summary, err error) {
	result, err := cli.ContainerList(ctx, client.ContainerListOptions{
		Filters: make(client.Filters).Add("label", key+"="+value),
		All:     false,
	})
	if err != nil {
		return nil, err
	}

	return result.Items, nil
}

// GetLabeledServices retrieves all services with a specific label key and value, along with their labels.
func GetLabeledServices(ctx context.Context, cli *client.Client, key, value string) (map[Service]map[string]string, error) {
	if swarmInternal.ModeEnabled {
		services, err := swarmInternal.GetServicesByLabel(ctx, cli, key, value)
		if err != nil {
			return nil, fmt.Errorf("failed to get services with label %s=%s: %w", key, value, err)
		}

		result := make(map[Service]map[string]string)
		for _, service := range services {
			result[Service(service.Spec.Name)] = service.Spec.TaskTemplate.ContainerSpec.Labels
		}

		return result, nil
	}

	containers, err := GetLabeledContainers(ctx, cli, key, value)
	if err != nil {
		return nil, fmt.Errorf("failed to get containers with label %s=%s: %w", key, value, err)
	}

	result := make(map[Service]map[string]string)
	for _, cont := range containers {
		result[Service(cont.Names[0])] = cont.Labels
	}

	return result, nil
}

// GetLabeledVolumes retrieves all volumes with a specific label key and value.
func GetLabeledVolumes(ctx context.Context, cli *client.Client, key, value string) (volumes []volume.Volume, err error) {
	volResp, err := cli.VolumeList(ctx, client.VolumeListOptions{
		Filters: make(client.Filters).Add("label", key+"="+value),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list volumes with label %s=%s: %w", key, value, err)
	}

	return volResp.Items, nil
}

// GetLabeledConfigs retrieves all configs with a specific label key and value.
func GetLabeledConfigs(ctx context.Context, cli *client.Client, key, value string) (configs []swarm.Config, err error) {
	result, err := cli.ConfigList(ctx, client.ConfigListOptions{
		Filters: make(client.Filters).Add("label", key+"="+value),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list configs with label %s=%s: %w", key, value, err)
	}

	return result.Items, nil
}

// GetLabeledSecrets retrieves all secrets with a specific label key and value.
func GetLabeledSecrets(ctx context.Context, cli *client.Client, key, value string) (secrets []swarm.Secret, err error) {
	result, err := cli.SecretList(ctx, client.SecretListOptions{
		Filters: make(client.Filters).Add("label", key+"="+value),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list secrets with label %s=%s: %w", key, value, err)
	}

	return result.Items, nil
}

// GetMountPointByDestination retrieves the mount point of a container volume/bind mount by its destination (mount point inside the container).
func GetMountPointByDestination(cli *client.Client, containerID, destination string) (container.MountPoint, error) {
	// Get the container info
	result, err := cli.ContainerInspect(context.TODO(), containerID, client.ContainerInspectOptions{})
	if err != nil {
		return container.MountPoint{}, fmt.Errorf("failed to inspect container %s: %w", containerID, err)
	}

	// Get the volume path
	for _, mount := range result.Container.Mounts {
		if mount.Destination == destination {
			return mount, nil
		}
	}

	return container.MountPoint{}, fmt.Errorf("%w: %s", ErrMountPointNotFound, destination)
}

// CheckMountPointWriteable checks if a mount point is writable by attempting to create a file in it.
func CheckMountPointWriteable(mountPoint container.MountPoint) error {
	if !mountPoint.RW {
		return fmt.Errorf("%w: %s", ErrMountPointNotWriteable, mountPoint.Destination)
	}

	// Create a test file to check if the mount point is writable
	testFilePath := filepath.Join(mountPoint.Destination, ".test")

	f, err := os.Create(testFilePath)
	if err != nil {
		return fmt.Errorf("failed to create file in %s: %w", testFilePath, err)
	}

	defer f.Close()

	defer func() {
		err = os.Remove(testFilePath)
		if err != nil {
			fmt.Printf("failed to remove test file %s: %v\n", testFilePath, err)
		}
	}()

	return nil
}

func RemoveLabeledVolumes(ctx context.Context, dockerClient *client.Client, stackName string) error {
	filterLabel := api.ProjectLabel
	if swarmInternal.ModeEnabled {
		filterLabel = swarmInternal.StackNamespaceLabel
	}

	volumes, err := GetLabeledVolumes(ctx, dockerClient, filterLabel, stackName)
	if err != nil {
		return fmt.Errorf("failed to get labeled volumes: %w", err)
	}

	for _, vol := range volumes {
		retries := 5

		for i := 0; i < retries; i++ {
			_, err = dockerClient.VolumeRemove(ctx, vol.Name, client.VolumeRemoveOptions{Force: true})
			if err != nil {
				if strings.Contains(err.Error(), ErrIsInUse.Error()) {
					time.Sleep(2 * time.Second)
					continue
				}

				return fmt.Errorf("failed to remove volume %s: %w", vol.Name, err)
			}

			break
		}
	}

	return nil
}
