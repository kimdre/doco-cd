package swarm

import (
	"context"
	"io"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/service/progress"
	"github.com/docker/docker/api/types/swarm"

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
