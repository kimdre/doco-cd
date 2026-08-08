package swarm

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/service/progress"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"golang.org/x/sync/errgroup"

	"github.com/kimdre/doco-cd/internal/docker/jsonstream"
)

const (
	rollbackStatusObservationTimeout  = 20 * time.Second
	rollbackStatusObservationInterval = 250 * time.Millisecond
)

// Service represents a service.
type Service struct {
	ID string
	swarm.Meta
	Spec         swarm.ServiceSpec
	PreviousSpec *swarm.ServiceSpec `json:",omitempty"`
	Endpoint     swarm.Endpoint
	UpdateStatus *swarm.UpdateStatus `json:",omitempty"`

	// ServiceStatus is an optional, extra field indicating the number of
	// desired and running tasks. It is provided primarily as a shortcut to
	// calculating these values client-side, which otherwise would require
	// listing all tasks for a service, an operation that could be
	// computation and network expensive.
	ServiceStatus *swarm.ServiceStatus `json:",omitempty"`

	// JobStatus is the status of a Service which is in one of ReplicatedJob or
	// GlobalJob modes. It is absent on Replicated and Global services.
	JobStatus *swarm.JobStatus `json:",omitempty"`
}

// waitOnService waits for the service to converge. It outputs a progress bar,
// if appropriate based on the CLI flags.
func waitOnService(ctx context.Context, dockerCli command.Cli, serviceID string) error {
	errChan := make(chan error, 1)

	pipeReader, pipeWriter := io.Pipe()
	defer pipeReader.Close() // nolint:errcheck

	go func() {
		errChan <- progress.ServiceProgress(ctx, dockerCli.Client(), serviceID, pipeWriter)

		defer pipeWriter.Close() //nolint:errcheck
	}()

	// Monitor the output of the progress reader for errors.
	progressErr := jsonstream.ErrorReader(ctx, pipeReader)
	if progressErr == nil {
		progressErr = <-errChan
	}

	serviceResult, err := dockerCli.Client().ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
	if err != nil {
		if progressErr != nil {
			return progressErr
		}

		return fmt.Errorf("failed to inspect service %s update status: %w", serviceID, err)
	}

	rollbackErr := rollbackUpdateStatusError(serviceID, serviceResult.Service.Spec.Name, serviceResult.Service.UpdateStatus)
	if rollbackErr != nil {
		return rollbackErr
	}

	if progressErr != nil {
		delayedRollbackErr := waitForRollbackUpdateStatus(
			ctx,
			dockerCli.Client(),
			serviceID,
			serviceResult.Service.Spec.Name,
			rollbackStatusObservationTimeout,
		)
		if delayedRollbackErr != nil {
			return delayedRollbackErr
		}
	}

	return progressErr
}

// waitForRollbackUpdateStatus keeps observing a service update for a short time
// to catch rollback states that may appear after the first progress error.
func waitForRollbackUpdateStatus(ctx context.Context, apiClient client.APIClient, serviceID, serviceName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		serviceResult, err := apiClient.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
		if err == nil {
			rollbackErr := rollbackUpdateStatusError(serviceID, serviceName, serviceResult.Service.UpdateStatus)
			if rollbackErr != nil {
				return rollbackErr
			}

			if status := serviceResult.Service.UpdateStatus; status != nil && isTerminalNonRollbackUpdateState(status.State) {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(rollbackStatusObservationInterval):
		}
	}

	return nil
}

// rollbackUpdateStatusError returns an error when a service update finished in a rollback state.
func rollbackUpdateStatusError(serviceID, serviceName string, status *swarm.UpdateStatus) error {
	if status == nil || !isRollbackUpdateState(status.State) {
		return nil
	}

	target := strings.TrimSpace(serviceName)
	if target == "" {
		target = serviceID
	}

	message := strings.TrimSpace(status.Message)
	if message == "" {
		return fmt.Errorf("service %s entered rollback state %q", target, status.State)
	}

	return fmt.Errorf("service %s entered rollback state %q: %s", target, status.State, message)
}

// isRollbackUpdateState reports whether the update state indicates a rollback lifecycle.
func isRollbackUpdateState(state swarm.UpdateState) bool {
	return state == swarm.UpdateStateRollbackStarted ||
		state == swarm.UpdateStateRollbackPaused ||
		state == swarm.UpdateStateRollbackCompleted
}

func isTerminalNonRollbackUpdateState(state swarm.UpdateState) bool {
	return state == swarm.UpdateStateCompleted || state == swarm.UpdateStatePaused
}

// waitForNetwork waits for the network to be ready by repeatedly inspecting it until it succeeds or times out.
func waitForNetwork(ctx context.Context, apiClient client.NetworkAPIClient, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := apiClient.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
		if err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	return fmt.Errorf("timeout waiting for network %s to be ready", name)
}

// waitForSecret waits for the secret to be ready by repeatedly inspecting it until it succeeds or times out.
func waitForSecret(ctx context.Context, apiClient client.SecretAPIClient, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := apiClient.SecretInspect(ctx, name, client.SecretInspectOptions{})
		if err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	return fmt.Errorf("timeout waiting for secret %s to be ready", name)
}

// waitForConfig waits for the config to be ready by repeatedly inspecting it until it succeeds or times out.
func waitForConfig(ctx context.Context, apiClient client.ConfigAPIClient, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := apiClient.ConfigInspect(ctx, name, client.ConfigInspectOptions{})
		if err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	return fmt.Errorf("timeout waiting for config %s to be ready", name)
}

// waitForResources waits for the specified resources to be ready by concurrently inspecting them until they succeed or time out.
func waitForResources(ctx context.Context, apiClient client.APIClient, networks map[string]client.NetworkCreateOptions, secrets []swarm.SecretSpec, configs []swarm.ConfigSpec) error {
	const resourceWaitTimeout = 5 * time.Second

	g, ctx := errgroup.WithContext(ctx)

	for name := range networks {
		g.Go(func() error {
			return waitForNetwork(ctx, apiClient, name, resourceWaitTimeout)
		})
	}

	for _, secret := range secrets {
		g.Go(func() error {
			return waitForSecret(ctx, apiClient, secret.Name, resourceWaitTimeout)
		})
	}

	for _, config := range configs {
		g.Go(func() error {
			return waitForConfig(ctx, apiClient, config.Name, resourceWaitTimeout)
		})
	}

	return g.Wait()
}
