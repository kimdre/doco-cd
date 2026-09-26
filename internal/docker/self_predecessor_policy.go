package docker

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// validatePredecessorRestartPolicy checks the policy Docker will actually use
// if the running predecessor must exit during recovery.
func validatePredecessorRestartPolicy(ctx context.Context, apiClient client.APIClient, containerID string) error {
	if containerID == "" {
		return fmt.Errorf("%w: the running doco-cd container ID is unknown", selfupdate.ErrUnsupported)
	}

	result, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect running doco-cd container restart policy: %w", err)
	}

	if result.Container.HostConfig == nil {
		return fmt.Errorf("%w: running doco-cd container %s has no host configuration", selfupdate.ErrUnsupported, containerID)
	}

	policy := result.Container.HostConfig.RestartPolicy
	if policy.IsAlways() || policy.IsUnlessStopped() || policy.IsOnFailure() {
		return nil
	}

	return fmt.Errorf("%w: running doco-cd container %s needs a restart policy before it can update itself", selfupdate.ErrUnsupported, containerID)
}
