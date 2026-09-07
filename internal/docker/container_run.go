package docker

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/containerd/errdefs"
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
	return fmt.Sprintf("container %s exited with status %d", e.ContainerID, e.ExitCode)
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

// RestartContainerAndWait starts or restarts the container (identically to
// RestartContainer) and then blocks until the container exits. This is the
// blocking variant of RestartContainer used when stop_services requires
// knowing when the job has finished.
//
// For a stopped container the wait is subscribed BEFORE starting to avoid
// racing with a fast-exiting container. For a running container,
// ContainerRestart is called first (it blocks until the container is running
// again after the restart cycle), and then the wait is subscribed.
func RestartContainerAndWait(ctx context.Context, apiClient client.APIClient, containerID string) error {
	inspectResult, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect container %s: %w", containerID, err)
	}

	if getContainerRunAction(inspectResult.Container) == containerRunActionStart {
		// Container is stopped. Subscribe to wait BEFORE starting so we don't
		// race with a fast-exiting container.
		waitResult := apiClient.ContainerWait(ctx, containerID, client.ContainerWaitOptions{Condition: containerTypes.WaitConditionNextExit})

		if _, err = apiClient.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
			return fmt.Errorf("start container %s: %w", containerID, err)
		}

		return awaitContainerExit(waitResult, containerID)
	}

	// Container is running. ContainerRestart stops and starts it in one API
	// call; subscribing to WaitConditionNextExit BEFORE the restart would
	// fire on the stop step (not the eventual job completion). Restart first
	// which blocks until the container is running again, then subscribe.
	if _, err = apiClient.ContainerRestart(ctx, containerID, client.ContainerRestartOptions{}); err != nil {
		return fmt.Errorf("restart container %s: %w", containerID, err)
	}

	waitResult := apiClient.ContainerWait(ctx, containerID, client.ContainerWaitOptions{Condition: containerTypes.WaitConditionNextExit})

	return awaitContainerExit(waitResult, containerID)
}

// awaitContainerExit waits for a ContainerWait response and translates it into
// an error, returning nil on a clean zero-exit.
func awaitContainerExit(waitResult client.ContainerWaitResult, containerID string) error {
	select {
	case waitErr := <-waitResult.Error:
		if waitErr != nil {
			return fmt.Errorf("wait for container %s: %w", containerID, waitErr)
		}
	case waitStatus := <-waitResult.Result:
		if waitStatus.Error != nil && waitStatus.Error.Message != "" {
			return fmt.Errorf("container %s failed: %s", containerID, waitStatus.Error.Message)
		}

		if waitStatus.StatusCode != 0 {
			return &ContainerExitError{ContainerID: containerID, ExitCode: int(waitStatus.StatusCode)}
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
	return RunContainerOneOffFromExistingWithOptions(ctx, apiClient, containerID, OneOffContainerOptions{})
}

type OneOffContainerOptions struct {
	RunID       string
	SourceID    string
	ScheduledAt string
	StartedAt   string
}

// RunContainerOneOffFromExistingWithOptions starts a disposable clone of
// containerID. A run ID retains the completed clone for crash recovery; callers
// must remove it with RemoveOneOffContainer when finalization is complete.
func RunContainerOneOffFromExistingWithOptions(ctx context.Context, apiClient client.APIClient, containerID string, opts OneOffContainerOptions) error {
	inspectResult, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect container %s: %w", containerID, err)
	}

	if inspectResult.Container.Config == nil {
		return fmt.Errorf("container %s has no config", containerID)
	}

	configCopy := *inspectResult.Container.Config

	configCopy.Labels = maps.Clone(configCopy.Labels)
	if configCopy.Labels == nil {
		configCopy.Labels = map[string]string{}
	}

	config := &configCopy

	config.Labels[DocoCDJobLabels.JobEphemeral] = "true"
	if opts.RunID != "" {
		config.Labels[DocoCDJobLabels.JobRunID] = opts.RunID
	}

	if opts.SourceID != "" {
		config.Labels[DocoCDJobLabels.JobSourceServiceID] = opts.SourceID
	}

	if opts.ScheduledAt != "" {
		config.Labels[DocoCDJobLabels.JobScheduledAt] = opts.ScheduledAt
	}

	if opts.StartedAt != "" {
		config.Labels[DocoCDJobLabels.JobStartedAt] = opts.StartedAt
	}

	var hostConfig *containerTypes.HostConfig

	if inspectResult.Container.HostConfig != nil {
		hostConfigCopy := *inspectResult.Container.HostConfig
		hostConfig = &hostConfigCopy
		hostConfig.RestartPolicy = containerTypes.RestartPolicy{Name: "no"}
		hostConfig.AutoRemove = opts.RunID == ""
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
	// (auto-removed) container. See RestartContainerAndWait for the reasoning
	// behind WaitConditionNextExit vs WaitConditionNotRunning.
	waitResult := apiClient.ContainerWait(ctx, createResult.ID, client.ContainerWaitOptions{Condition: containerTypes.WaitConditionNextExit})

	if _, err = apiClient.ContainerStart(ctx, createResult.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start one-off container %s: %w", createResult.ID, err)
	}

	return awaitContainerExit(waitResult, createResult.ID)
}

// WaitOnOneOffContainer waits for a retained container to stop and returns its
// exit result. It is safe to call for an already-exited container.
func WaitOnOneOffContainer(ctx context.Context, apiClient client.APIClient, containerID string) error {
	inspectResult, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect one-off container %s: %w", containerID, err)
	}

	if inspectResult.Container.State == nil {
		return fmt.Errorf("one-off container %s has no state", containerID)
	}

	if !inspectResult.Container.State.Running {
		if inspectResult.Container.State.Status == containerTypes.StateCreated {
			return fmt.Errorf("one-off container %s was created but never started", containerID)
		}

		if inspectResult.Container.State.ExitCode != 0 {
			return &ContainerExitError{ContainerID: containerID, ExitCode: inspectResult.Container.State.ExitCode}
		}

		return nil
	}

	waitResult := apiClient.ContainerWait(ctx, containerID, client.ContainerWaitOptions{Condition: containerTypes.WaitConditionNotRunning})

	return awaitContainerExit(waitResult, containerID)
}

// FindOneOffContainer finds the one retained ephemeral container for runID.
func FindOneOffContainer(ctx context.Context, apiClient client.APIClient, runID string) (string, error) {
	filter := make(client.Filters)
	filter.Add("label", DocoCDJobLabels.JobRunID+"="+runID)

	result, err := apiClient.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	if err != nil {
		return "", fmt.Errorf("list one-off containers for run %s: %w", runID, err)
	}

	if len(result.Items) == 0 {
		return "", nil
	}

	if len(result.Items) != 1 {
		return "", fmt.Errorf("found %d one-off containers for run %s", len(result.Items), runID)
	}

	return result.Items[0].ID, nil
}

// RemoveOneOffContainer removes a retained one-off container after reporting.
func RemoveOneOffContainer(ctx context.Context, apiClient client.APIClient, containerID string) error {
	_, err := apiClient.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove one-off container %s: %w", containerID, err)
	}

	return nil
}
