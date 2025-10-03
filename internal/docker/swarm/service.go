package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/service/progress"
	"github.com/docker/docker/api/types/swarm"
)

var ErrNoSuchImage = errors.New("no such image")

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
	type progressLine struct {
		Status string `json:"status"`
	}

	errChan := make(chan error, 1)
	imageNotFoundChan := make(chan string, 1)

	defer close(imageNotFoundChan)
	defer close(errChan)

	pipeReader, pipeWriter := io.Pipe()

	go func() {
		errChan <- progress.ServiceProgress(ctx, dockerCli.Client(), serviceID, pipeWriter)
	}()

	// Monitor for "No such image" errors
	go func() {
		defer pipeReader.Close() // nolint:errcheck

		dec := json.NewDecoder(pipeReader)
		count := 0
		var lastImage string

		for {
			var line progressLine
			if err := dec.Decode(&line); err != nil {
				if err == io.EOF {
					break
				}
				continue // skip malformed lines
			}
			fmt.Println(line.Status) // for debugging
			if idx := strings.Index(line.Status, "No such image:"); idx != -1 {
				image := strings.TrimSpace(line.Status[idx+len("No such image:"):])
				lastImage = image
				count++
				fmt.Println("Hit: ", count) // for debugging
				if count >= 3 {
					imageNotFoundChan <- lastImage
					return
				}
			}
		}
	}()

	go io.Copy(io.Discard, pipeReader) // nolint:errcheck

	select {
	case img := <-imageNotFoundChan:
		return fmt.Errorf("%w: %s", ErrNoSuchImage, img)
	case err := <-errChan:
		return err
	}
}
