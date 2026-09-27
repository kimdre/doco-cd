package selfupdate

import (
	"context"
	"fmt"
	"strconv"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"
)

// Identity describes this doco-cd container's place in a compose project.
type Identity struct {
	// OK is false when this process does not run in a compose-managed container.
	OK            bool
	ContainerID   string
	ContainerName string
	Project       string
	Service       string
	Number        int
	Image         string
	Labels        map[string]string
}

// Detect inspects the own container and derives its compose identity.
func Detect(ctx context.Context, apiClient client.APIClient, containerID string) (Identity, error) {
	if containerID == "" {
		return Identity{}, nil
	}

	result, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return Identity{}, fmt.Errorf("inspect own container %s: %w", containerID, err)
	}

	if result.Container.Config == nil {
		return Identity{}, fmt.Errorf("own container %s has no config", containerID)
	}

	name := result.Container.Name
	if len(name) > 0 && name[0] == '/' {
		name = name[1:]
	}

	id := IdentityFromLabels(containerID, name, result.Container.Config.Labels)
	id.Image = result.Container.Config.Image

	return id, nil
}

// IdentityFromLabels builds an Identity from a container's labels.
func IdentityFromLabels(containerID, containerName string, labels map[string]string) Identity {
	id := Identity{
		ContainerID:   containerID,
		ContainerName: containerName,
		Project:       labels[api.ProjectLabel],
		Service:       labels[api.ServiceLabel],
		Labels:        labels,
	}

	if number, err := strconv.Atoi(labels[api.ContainerNumberLabel]); err == nil {
		id.Number = number
	}

	id.OK = id.Project != "" && id.Service != ""

	return id
}
