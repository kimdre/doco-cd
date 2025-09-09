package swarm

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"gotest.tools/v3/assert"
)

// FakeClient is a fake NetworkAPIClient.
type FakeClient struct {
	client.NetworkAPIClient
	NetworkInspectFunc func(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error)
}

// NetworkInspect fakes inspecting a network.
func (c *FakeClient) NetworkInspect(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error) {
	if c.NetworkInspectFunc != nil {
		return c.NetworkInspectFunc(ctx, networkID, options)
	}

	return network.Inspect{}, nil
}

type notFoundError struct {
	error
}

func (notFoundError) NotFound() {}

func TestValidateExternalNetworks(t *testing.T) {
	if !ModeEnabled {
		t.Skip("Swarm mode not enabled, skipping test")
	}

	testCases := []struct {
		inspectResponse network.Inspect
		inspectError    error
		expectedMsg     string
		network         string
	}{
		{
			inspectError: notFoundError{},
			expectedMsg:  "could not be found. You need to create a swarm-scoped network",
		},
		{
			inspectError: errors.New("unexpected"),
			expectedMsg:  "unexpected",
		},
		// FIXME(vdemeester) that doesn't work under windows, the check needs to be smarter
		/*
			{
				inspectError: errors.New("host net does not exist on swarm classic"),
				network:      "host",
			},
		*/
		{
			network:     "user",
			expectedMsg: "is not in the right scope",
		},
		{
			network:         "user",
			inspectResponse: network.Inspect{Scope: "swarm"},
		},
	}

	for _, testcase := range testCases {
		c := &FakeClient{
			NetworkInspectFunc: func(_ context.Context, _ string, _ network.InspectOptions) (network.Inspect, error) {
				return testcase.inspectResponse, testcase.inspectError
			},
		}
		networks := []string{testcase.network}

		err := validateExternalNetworks(context.Background(), c, networks)
		if testcase.expectedMsg == "" {
			assert.NilError(t, err)
		} else {
			assert.ErrorContains(t, err, testcase.expectedMsg)
		}
	}
}
