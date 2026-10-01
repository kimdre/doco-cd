package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/lifecycle"
)

type healthTestClient struct {
	client.APIClient
	status container.HealthStatus
	err    error
}

func (c *healthTestClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: container.InspectResponse{
		State: &container.State{Running: true, Health: &container.Health{Status: c.status}},
	}}, c.err
}

func TestWaitHealthyFailureCauses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		status     container.HealthStatus
		inspectErr error
		timeout    time.Duration
		unhealthy  bool
		timedOut   bool
	}{
		{name: "health deadline", status: container.Starting, timeout: time.Millisecond, unhealthy: true, timedOut: true},
		{name: "Docker unhealthy", status: container.Unhealthy, timeout: time.Minute, unhealthy: true},
		{name: "Docker deadline", inspectErr: fmt.Errorf("Docker request: %w", context.DeadlineExceeded), timeout: time.Minute, timedOut: true},
		{name: "Docker cancellation", inspectErr: context.Canceled, timeout: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := WaitHealthy(t.Context(), &healthTestClient{status: tc.status, err: tc.inspectErr}, "new", tc.timeout, nil)
			if err == nil {
				t.Fatal("expected a health failure")
			}

			if errors.Is(err, ErrUnhealthy) != tc.unhealthy || lifecycle.IsTimeout(err) != tc.timedOut {
				t.Errorf("WaitHealthy() = %v; want unhealthy=%t, timedOut=%t", err, tc.unhealthy, tc.timedOut)
			}

			if tc.inspectErr == nil && lifecycle.IsCancellation(err) {
				t.Errorf("WaitHealthy() = %v; health failures must not be lifecycle cancellation", err)
			}

			if tc.inspectErr != nil && !errors.Is(err, tc.inspectErr) {
				t.Errorf("WaitHealthy() lost the Docker error: %v", err)
			}
		})
	}
}

func TestWaitHealthyPreservesCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := WaitHealthy(ctx, &healthTestClient{status: container.Starting}, "new", time.Minute, nil)
	if !errors.Is(err, context.Canceled) || lifecycle.IsTimeout(err) {
		t.Errorf("WaitHealthy() = %v; want cancellation without timeout", err)
	}
}
