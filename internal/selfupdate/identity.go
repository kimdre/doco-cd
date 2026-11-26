package selfupdate

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"
)

// Labels that Docker sets on Swarm task containers.
const (
	swarmStackNamespaceLabel   = "com.docker.stack.namespace"
	swarmServiceNameLabel      = "com.docker.swarm.service.name"
	swarmTaskNameLabel         = "com.docker.swarm.task.name"
	swarmServiceIDRuntimeLabel = "com.docker.swarm.service.id"
	swarmTaskIDRuntimeLabel    = "com.docker.swarm.task.id"
)

// Identity describes this doco-cd container's place in a compose project or Swarm stack.
type Identity struct {
	// OK is false when this process runs neither in a compose-managed container
	// nor in a Swarm stack task.
	OK bool
	// Swarm is true when the identity comes from Swarm stack task labels.
	// Project is then the stack name and Service the service name without the stack prefix.
	Swarm          bool
	SwarmServiceID string
	SwarmTaskID    string
	ContainerID    string
	ContainerName  string
	Project        string
	Service        string
	Number         int
	Image          string
	Labels         map[string]string
}

// Detect inspects the own container and derives its Compose or Swarm identity.
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

	if swarmID := swarmIdentityFromLabels(id, labels); swarmID.OK {
		return swarmID
	}

	if number, err := strconv.Atoi(labels[api.ContainerNumberLabel]); err == nil {
		id.Number = number
	}

	id.OK = id.Project != "" && id.Service != ""

	return id
}

// swarmIdentityFromLabels derives the identity of a Swarm stack task container.
// Docker's Swarm labels take priority because Compose labels can be inherited from the service config.
func swarmIdentityFromLabels(id Identity, labels map[string]string) Identity {
	stack := labels[swarmStackNamespaceLabel]
	service := strings.TrimPrefix(labels[swarmServiceNameLabel], stack+"_")

	if stack == "" || service == "" || service == labels[swarmServiceNameLabel] {
		return id
	}

	id.Project = stack
	id.Service = service
	id.Number = 0
	id.Swarm = true
	id.SwarmServiceID = labels[swarmServiceIDRuntimeLabel]
	id.SwarmTaskID = labels[swarmTaskIDRuntimeLabel]
	id.OK = true

	// The task name has the format <service>.<slot>.<task id>. Global services use the node ID as slot.
	taskName, matchesService := strings.CutPrefix(labels[swarmTaskNameLabel], labels[swarmServiceNameLabel]+".")
	if !matchesService {
		return id
	}

	if slot, _, found := strings.Cut(taskName, "."); found {
		if number, err := strconv.Atoi(slot); err == nil && number > 0 {
			id.Number = number
		}
	}

	return id
}
