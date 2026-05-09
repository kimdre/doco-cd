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

func RunContainerOneShotFromExisting(ctx context.Context, apiClient client.APIClient, containerID string) error {
	inspectResult, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect container %s: %w", containerID, err)
	}

	if inspectResult.Container.Config == nil {
		return fmt.Errorf("container %s has no config", containerID)
	}

	config := inspectResult.Container.Config

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
		return fmt.Errorf("create one-shot container from %s: %w", containerID, err)
	}

	if _, err = apiClient.ContainerStart(ctx, createResult.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start one-shot container %s: %w", createResult.ID, err)
	}

	waitResult := apiClient.ContainerWait(ctx, createResult.ID, client.ContainerWaitOptions{Condition: containerTypes.WaitConditionNotRunning})
	select {
	case waitErr := <-waitResult.Error:
		if waitErr != nil {
			return fmt.Errorf("wait for one-shot container %s: %w", createResult.ID, waitErr)
		}
	case waitStatus := <-waitResult.Result:
		if waitStatus.Error != nil && waitStatus.Error.Message != "" {
			return fmt.Errorf("one-shot container %s failed: %s", createResult.ID, waitStatus.Error.Message)
		}

		if waitStatus.StatusCode != 0 {
			return fmt.Errorf("one-shot container %s exited with status %d", createResult.ID, waitStatus.StatusCode)
		}
	}

	return nil
}
