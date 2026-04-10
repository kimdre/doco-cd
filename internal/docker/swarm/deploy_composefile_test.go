package swarm

import (
	"context"
	"errors"
	"slices"
	"testing"

	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"gotest.tools/v3/assert"
)

// FakeClient is a fake NetworkAPIClient.
type FakeClient struct {
	client.NetworkAPIClient
	NetworkInspectFunc func(ctx context.Context, networkID string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error)
}

// NetworkInspect fakes inspecting a network.
func (c *FakeClient) NetworkInspect(ctx context.Context, networkID string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	if c.NetworkInspectFunc != nil {
		return c.NetworkInspectFunc(ctx, networkID, options)
	}

	return client.NetworkInspectResult{}, nil
}

type notFoundError struct {
	error
}

func (notFoundError) NotFound() {}

func TestValidateExternalNetworks(t *testing.T) {
	t.Parallel()

	if err := RefreshModeEnabled(t.Context(), getDockerCli(t)); err != nil {
		t.Fatalf("failed refreshing mode enabled: %v", err)
	}

	if !GetModeEnabled() {
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
		{
			network:     "user",
			expectedMsg: "is not in the right scope",
		},
		{
			network:         "user",
			inspectResponse: network.Inspect{Network: network.Network{Scope: "swarm"}},
		},
	}

	for _, testcase := range testCases {
		c := &FakeClient{
			NetworkInspectFunc: func(_ context.Context, _ string, _ client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
				return client.NetworkInspectResult{Network: testcase.inspectResponse}, testcase.inspectError
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

func TestGetDeclaredNetworks(t *testing.T) {
	t.Parallel()

	serviceConfigs := []composetypes.ServiceConfig{
		{
			Name: "web",
			Networks: map[string]*composetypes.ServiceNetworkConfig{
				"frontend": {},
			},
		},
		{
			Name: "api",
		},
	}

	networkConfigs := map[string]composetypes.NetworkConfig{
		"frontend": {},
		"backend":  {},
	}

	t.Run("all resources disabled", func(t *testing.T) {
		networks := getDeclaredNetworks(serviceConfigs, networkConfigs, false).ToSlice()
		slices.Sort(networks)

		assert.DeepEqual(t, networks, []string{"default", "frontend"})
	})

	t.Run("all resources enabled", func(t *testing.T) {
		networks := getDeclaredNetworks(serviceConfigs, networkConfigs, true).ToSlice()
		slices.Sort(networks)

		assert.DeepEqual(t, networks, []string{"backend", "default", "frontend"})
	})
}

func TestGetDeclaredSecrets(t *testing.T) {
	t.Parallel()

	serviceConfigs := []composetypes.ServiceConfig{
		{
			Name: "api",
			Secrets: []composetypes.ServiceSecretConfig{
				{Source: "db_password"},
			},
		},
	}

	secretConfigs := map[string]composetypes.SecretConfig{
		"db_password":   {},
		"unused_secret": {},
	}

	t.Run("all resources disabled", func(t *testing.T) {
		secrets := getDeclaredSecrets(serviceConfigs, secretConfigs, false)

		_, hasReferenced := secrets["db_password"]
		_, hasUnused := secrets["unused_secret"]

		assert.Assert(t, hasReferenced)
		assert.Assert(t, !hasUnused)
	})

	t.Run("all resources enabled", func(t *testing.T) {
		secrets := getDeclaredSecrets(serviceConfigs, secretConfigs, true)

		assert.Equal(t, len(secrets), 2)
	})
}

func TestGetDeclaredConfigs(t *testing.T) {
	t.Parallel()

	serviceConfigs := []composetypes.ServiceConfig{
		{
			Name: "api",
			Configs: []composetypes.ServiceConfigObjConfig{
				{Source: "app_cfg"},
			},
		},
	}

	configConfigs := map[string]composetypes.ConfigObjConfig{
		"app_cfg":       {},
		"unused_config": {},
	}

	t.Run("all resources disabled", func(t *testing.T) {
		configs := getDeclaredConfigs(serviceConfigs, configConfigs, false)

		_, hasReferenced := configs["app_cfg"]
		_, hasUnused := configs["unused_config"]

		assert.Assert(t, hasReferenced)
		assert.Assert(t, !hasUnused)
	})

	t.Run("all resources enabled", func(t *testing.T) {
		configs := getDeclaredConfigs(serviceConfigs, configConfigs, true)

		assert.Equal(t, len(configs), 2)
	})
}
