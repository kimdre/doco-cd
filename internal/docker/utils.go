package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

var (
	ErrMountPointNotFound     = errors.New("mount point not found")
	ErrMountPointNotWriteable = errors.New("mount point is not writeable")
	ErrContainerIDNotFound    = errors.New("container ID not found")
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

// SetConfigHashPrefixes generates hashes for the config definitions in the compose project
// and adds them to the config names as suffixes to trigger a redeployment when they change (Only works in Docker Swarm mode).
func SetConfigHashPrefixes(project *types.Project) error {
	for i, c := range project.Configs {
		if c.External {
			// Skip external configs, they are not managed by compose
			continue
		}

		var content io.Reader

		switch {
		case c.File != "":
			// Config content is created from a file
			contentBytes, err := os.ReadFile(c.File)
			if err != nil {
				return fmt.Errorf("failed to read config file %s: %w", c.File, err)
			}

			content = strings.NewReader(string(contentBytes))

		case c.Content != "":
			// Config content is created with the inlined value
			content = strings.NewReader(c.Content)

		case c.Environment != "":
			// Config content is created from environment variables.
			// Not supported because doco-cd cannot reach env from the docker host.
			return fmt.Errorf("config %s uses a environment variable, which is not supported by doco-cd: %s", c.Name, c.Environment)

		default:
			continue // Skip configs without content
		}

		hash, err := generateShortHash(content)
		if err != nil {
			return fmt.Errorf("failed to generate hash for config %s: %w", c.Name, err)
		}

		nameWithHash := fmt.Sprintf("%s_%s", c.Name, hash)
		c.Name = nameWithHash
		project.Configs[i] = c

		// Check for services that use this config and update their config references
		for j, service := range project.Services {
			for k, config := range service.Configs {
				if config.Source == c.Name {
					// Update the config reference in the service
					project.Services[j].Configs[k].Source = nameWithHash
				}
			}
		}
	}

	return nil
}

// SetSecretHashPrefixes generates hashes for the secret definitions in the compose project
// and adds them to the secret names as suffixes to trigger a redeployment when they change (Only works in Docker Swarm mode).
func SetSecretHashPrefixes(project *types.Project) error {
	for i, s := range project.Secrets {
		if s.External {
			// Skip external secrets, they are not managed by compose
			continue
		}

		var content io.Reader

		switch {
		case s.File != "":
			// Secret content is created from a file
			contentBytes, err := os.ReadFile(s.File)
			if err != nil {
				return fmt.Errorf("failed to read secret file %s: %w", s.File, err)
			}

			content = strings.NewReader(string(contentBytes))

		case s.Content != "":
			// Secret content is created with the inlined value
			content = strings.NewReader(s.Content)

		case s.Environment != "":
			// Secret content is created from environment variables.
			// Not supported because doco-cd cannot reach env from the docker host.
			return fmt.Errorf("secret %s uses a environment variable, which is not supported by doco-cd: %s", s.Name, s.Environment)

		default:
			continue // Skip secrets without content
		}

		hash, err := generateShortHash(content)
		if err != nil {
			return fmt.Errorf("failed to generate hash for secret %s: %w", s.Name, err)
		}

		nameWithHash := fmt.Sprintf("%s_%s", s.Name, hash)
		s.Name = nameWithHash
		project.Secrets[i] = s

		// Check for services that use this secret and update their secret references
		for j, service := range project.Services {
			for k, secret := range service.Secrets {
				if secret.Source == s.Name {
					// Update the secret reference in the service
					project.Services[j].Secrets[k].Source = nameWithHash
				}
			}
		}
	}

	return nil
}

// generateShortHash generates a short hash from the provided data reader.
func generateShortHash(data io.Reader) (hash string, err error) {
	const length = 10 // Desired length of the hash

	h := sha256.New()

	_, err = io.Copy(h, data)
	if err != nil {
		return "", fmt.Errorf("failed to generate hash: %w", err)
	}

	hash = hex.EncodeToString(h.Sum(nil))
	if len(hash) > length {
		hash = hash[:length] // Shorten hash to n characters
	} else if hash == "" {
		return "", errors.New("hash is empty")
	}

	return hash, nil
}
