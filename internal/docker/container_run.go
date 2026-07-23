package docker

import (
	"context"
	"fmt"
	"strings"
	"time"

	containerTypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

type containerRunAction string

const (
	containerRunActionStart   containerRunAction = "start"
	containerRunActionRestart containerRunAction = "restart"
)

// ContainerExitError reports that a container finished with a non-zero exit code.
// Callers can use errors.As to recover the exit code without parsing error strings.
type ContainerExitError struct {
	ContainerID string
	ExitCode    int
}

func (e *ContainerExitError) Error() string {
	return fmt.Sprintf("one-off container %s exited with status %d", e.ContainerID, e.ExitCode)
}

func RestartContainer(ctx context.Context, apiClient client.APIClient, containerID string) error {
	inspectResult, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect container %s: %w", containerID, err)
	}

	switch getContainerRunAction(inspectResult.Container) {
	case containerRunActionStart:
		if _, err = apiClient.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
			return fmt.Errorf("start container %s: %w", containerID, err)
		}
	default:
		if _, err = apiClient.ContainerRestart(ctx, containerID, client.ContainerRestartOptions{}); err != nil {
			return fmt.Errorf("restart container %s: %w", containerID, err)
		}
	}

	return nil
}

func getContainerRunAction(inspectResult containerTypes.InspectResponse) containerRunAction {
	if inspectResult.State == nil {
		return containerRunActionRestart
	}

	if !inspectResult.State.Running {
		return containerRunActionStart
	}

	return containerRunActionRestart
}

func RunContainerOneOffFromExisting(ctx context.Context, apiClient client.APIClient, containerID string) error {
	inspectResult, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect container %s: %w", containerID, err)
	}

	if inspectResult.Container.Config == nil {
		return fmt.Errorf("container %s has no config", containerID)
	}

	config := inspectResult.Container.Config
	if config.Labels == nil {
		config.Labels = map[string]string{}
	}

	config.Labels[DocoCDJobLabels.JobEphemeral] = "true"

	hostConfig := inspectResult.Container.HostConfig
	if hostConfig != nil {
		hostConfig.RestartPolicy = containerTypes.RestartPolicy{Name: "no"}
		hostConfig.AutoRemove = true
	}

	baseName := strings.TrimPrefix(inspectResult.Container.Name, "/")

	baseName = strings.ReplaceAll(baseName, "/", "-")
	if baseName == "" {
		baseName = containerID[:12]
	}

	tmpName := fmt.Sprintf("%s-doco-job-%d", baseName, time.Now().UTC().UnixNano())

	createResult, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           config,
		HostConfig:       hostConfig,
		NetworkingConfig: &network.NetworkingConfig{},
		Name:             tmpName,
	})
	if err != nil {
		return fmt.Errorf("create one-off container from %s: %w", containerID, err)
	}

	// Subscribe to wait BEFORE starting so we don't race with a fast-exiting
	// (auto-removed) container: if ContainerStart is called first the container
	// may finish and be removed before ContainerWait registers, causing a
	// "No such container" error.
	waitResult := apiClient.ContainerWait(ctx, createResult.ID, client.ContainerWaitOptions{Condition: containerTypes.WaitConditionNotRunning})

	if _, err = apiClient.ContainerStart(ctx, createResult.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start one-off container %s: %w", createResult.ID, err)
	}

	select {
	case waitErr := <-waitResult.Error:
		if waitErr != nil {
			return fmt.Errorf("wait for one-off container %s: %w", createResult.ID, waitErr)
		}
	case waitStatus := <-waitResult.Result:
		if waitStatus.Error != nil && waitStatus.Error.Message != "" {
			return fmt.Errorf("one-off container %s failed: %s", createResult.ID, waitStatus.Error.Message)
		}

		if waitStatus.StatusCode != 0 {
			return &ContainerExitError{ContainerID: createResult.ID, ExitCode: int(waitStatus.StatusCode)}
		}
	}

	return nil
}
