package swarm

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/service/progress"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"golang.org/x/sync/errgroup"

	"github.com/kimdre/doco-cd/internal/docker/jsonstream"
)

// Service represents a service.
type Service struct {
	ID string
	swarm.Meta
	Spec         swarm.ServiceSpec   `json:",omitempty"`
	PreviousSpec *swarm.ServiceSpec  `json:",omitempty"`
	Endpoint     swarm.Endpoint      `json:",omitempty"`
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
	}()

	// Monitor the output of the progress reader for errors
	err := jsonstream.ErrorReader(ctx, pipeReader)
	if err == nil {
		err = <-errChan
	}

	return err
}

// waitForNetwork waits for the network to be ready by repeatedly inspecting it until it succeeds or times out.
func waitForNetwork(ctx context.Context, apiClient client.NetworkAPIClient, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := apiClient.NetworkInspect(ctx, name, network.InspectOptions{})
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
		_, _, err := apiClient.SecretInspectWithRaw(ctx, name)
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
		_, _, err := apiClient.ConfigInspectWithRaw(ctx, name)
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
func waitForResources(ctx context.Context, apiClient client.APIClient, networks map[string]network.CreateOptions, secrets []swarm.SecretSpec, configs []swarm.ConfigSpec) error {
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
