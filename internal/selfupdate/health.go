package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const (
	healthPollInterval  = 2 * time.Second
	healthySamplesWant  = 2
	healthExecStartWait = 500 * time.Millisecond
)

// ErrUnhealthy is returned when a container failed its health gate.
var ErrUnhealthy = errors.New("container did not become healthy")

// WaitHealthy blocks until containerID reports healthy twice in a row, or the
// timeout expires. Containers without a healthcheck are probed by running the
// binary's own healthcheck subcommand inside them.
func WaitHealthy(ctx context.Context, apiClient client.APIClient, containerID string, timeout time.Duration, log *slog.Logger) error {
	deadline := time.Now().Add(timeout)
	healthy := 0

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %s within %s", ErrUnhealthy, containerID, timeout)
		}

		ok, reason, err := sampleHealth(ctx, apiClient, containerID)
		if err != nil {
			return err
		}

		if ok {
			healthy++
			if healthy >= healthySamplesWant {
				return nil
			}
		} else {
			if healthy > 0 && log != nil {
				log.Debug("self-update: health sample reset", slog.String("container_id", containerID), slog.String("reason", reason))
			}

			healthy = 0
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(healthPollInterval):
		}
	}
}

// sampleHealth returns whether the container looks healthy right now. A hard
// unhealthy verdict from Docker ends the wait immediately.
func sampleHealth(ctx context.Context, apiClient client.APIClient, containerID string) (bool, string, error) {
	result, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return false, "", fmt.Errorf("inspect %s: %w", containerID, err)
	}

	state := result.Container.State
	if state == nil {
		return false, "no state", nil
	}

	if !state.Running {
		return false, "not running: " + string(state.Status), nil
	}

	if state.Health != nil {
		switch state.Health.Status {
		case container.Healthy:
			return true, "", nil
		case container.Unhealthy:
			return false, "", fmt.Errorf("%w: %s reported unhealthy", ErrUnhealthy, containerID)
		default:
			return false, "health " + string(state.Health.Status), nil
		}
	}

	return execHealthcheck(ctx, apiClient, containerID)
}

// execHealthcheck runs doco-cd's own healthcheck inside the container, for
// services that declare no healthcheck of their own.
func execHealthcheck(ctx context.Context, apiClient client.APIClient, containerID string) (bool, string, error) {
	created, err := apiClient.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd: []string{"/doco-cd", "healthcheck"},
	})
	if err != nil {
		return false, "", fmt.Errorf("create healthcheck exec in %s: %w", containerID, err)
	}

	if _, err = apiClient.ExecStart(ctx, created.ID, client.ExecStartOptions{}); err != nil {
		return false, "", fmt.Errorf("start healthcheck exec in %s: %w", containerID, err)
	}

	for {
		inspect, inspectErr := apiClient.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if inspectErr != nil {
			return false, "", fmt.Errorf("inspect healthcheck exec in %s: %w", containerID, inspectErr)
		}

		if !inspect.Running {
			if inspect.ExitCode == 0 {
				return true, "", nil
			}

			return false, fmt.Sprintf("healthcheck exit %d", inspect.ExitCode), nil
		}

		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-time.After(healthExecStartWait):
		}
	}
}
