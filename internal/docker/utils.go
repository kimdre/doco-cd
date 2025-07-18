package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

var (
	ErrMountPointNotFound     = errors.New("mount point not found")
	ErrMountPointNotWriteable = errors.New("mount point is not writeable")
	ErrContainerIDNotFound    = errors.New("container ID not found")
	ErrBuildInfoUnavailable   = errors.New("build info unavailable")
	ErrModuleNotFound         = errors.New("module not found in build info")
)

// GetContainerID retrieves the container ID for a given service name.
func GetContainerID(client client.APIClient, name string) (id string, err error) {
	containers, err := client.ContainerList(context.TODO(), container.ListOptions{All: true})
	if err != nil {
		return "", err
	}

	for _, cont := range containers {
		for _, containerName := range cont.Names {
			if strings.Contains(containerName, name) { // Match by service name
				return cont.ID, nil
			}
		}
	}

	return "", fmt.Errorf("%w: %s", ErrContainerIDNotFound, name)
}

// GetLabeledContainers retrieves all containers with a specific label key and value.
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

// GetMountPointByDestination retrieves the mount point of a container volume/bind mount by its destination (mount point inside the container).
func GetMountPointByDestination(cli *client.Client, containerID, destination string) (container.MountPoint, error) {
	// Get the container info
	cont, err := cli.ContainerInspect(context.TODO(), containerID)
	if err != nil {
		return container.MountPoint{}, fmt.Errorf("failed to inspect container %s: %w", containerID, err)
	}

	// Get the volume path
	for _, mount := range cont.Mounts {
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

	_, err := os.Create(testFilePath) // #nosec G304
	if err != nil {
		return fmt.Errorf("failed to create file in %s: %w", testFilePath, err)
	}

	defer func() {
		err = os.Remove(testFilePath)
		if err != nil {
			fmt.Printf("failed to remove test file %s: %v\n", testFilePath, err)
		}
	}()

	return nil
}

// GetModuleVersion retrieves the version of a specified module from the build info.
func GetModuleVersion(module string) (string, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ErrBuildInfoUnavailable
	}

	for _, dep := range info.Deps {
		if dep.Path == module {
			return strings.TrimPrefix(dep.Version, "v"), nil
		}
	}

	return "", fmt.Errorf("%w: %s", ErrModuleNotFound, module)
}
