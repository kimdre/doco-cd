package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/selfupdate"
)

type selfPolicyClient struct {
	client.APIClient
	hostConfig *container.HostConfig
	err        error
}

// ContainerInspect supplies the predecessor's running restart policy.
func (c selfPolicyClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: container.InspectResponse{HostConfig: c.hostConfig}}, c.err
}

// TestValidatePredecessorRestartPolicy verifies the live predecessor's policy
// rather than trusting the desired Compose configuration.
func TestValidatePredecessorRestartPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		policy          container.RestartPolicy
		noHostConfig    bool
		err             error
		wantUnsupported bool
	}{
		{name: "always", policy: container.RestartPolicy{Name: container.RestartPolicyAlways}},
		{name: "unless stopped", policy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}},
		{name: "on failure", policy: container.RestartPolicy{Name: container.RestartPolicyOnFailure}},
		{name: "new config cannot make old container restart", policy: container.RestartPolicy{Name: container.RestartPolicyDisabled}, wantUnsupported: true},
		{name: "no host configuration", noHostConfig: true, wantUnsupported: true},
		{name: "inspect error", err: errors.New("daemon unavailable")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			config := &container.HostConfig{RestartPolicy: tt.policy}
			if tt.noHostConfig {
				config = nil
			}

			err := validatePredecessorRestartPolicy(t.Context(), selfPolicyClient{
				hostConfig: config,
				err:        tt.err,
			}, "old")
			if (err != nil) != (tt.err != nil || tt.wantUnsupported) {
				t.Fatalf("validatePredecessorRestartPolicy() error = %v", err)
			}

			if errors.Is(err, selfupdate.ErrUnsupported) != tt.wantUnsupported {
				t.Errorf("unsupported = %v, want %v", errors.Is(err, selfupdate.ErrUnsupported), tt.wantUnsupported)
			}

			if tt.err != nil && !errors.Is(err, tt.err) {
				t.Errorf("inspect error not wrapped: %v", err)
			}
		})
	}
}
