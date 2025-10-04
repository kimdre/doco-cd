package swarm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/service/progress"
	"github.com/docker/docker/api/types/swarm"

	"github.com/kimdre/doco-cd/internal/docker/jsonstream"
)

var ErrImagePullAccessDenied = errors.New("image pull access denied")

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
	imageNotFoundChan := make(chan string, 1)

	pipeReader, pipeWriter := io.Pipe()
	defer pipeReader.Close() // nolint:errcheck

	go func() {
		errChan <- progress.ServiceProgress(ctx, dockerCli.Client(), serviceID, pipeWriter)
	}()

	// Monitor for "No such image" errors
	go func() {
		err := jsonstream.ErrorReader(ctx, pipeReader)
		if err != nil {
			// Check for "image not found" error and extract image name if needed
			if strings.HasPrefix(err.Error(), "image not found: ") {
				image := strings.TrimPrefix(err.Error(), "image not found: ")
				imageNotFoundChan <- image

				return
			}

			errChan <- err
		}
	}()

	select {
	case img := <-imageNotFoundChan:
		return fmt.Errorf("%w: %s, image does not exist or may require authentication", ErrImagePullAccessDenied, img)
	case err := <-errChan:
		return err
	}
}
