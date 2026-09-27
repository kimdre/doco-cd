package docker

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

type driftNetworkClient struct {
	client.APIClient
	network network.Inspect
}

// NetworkList supplies the existing project networks for drift detection.
func (c driftNetworkClient) NetworkList(context.Context, client.NetworkListOptions) (client.NetworkListResult, error) {
	return client.NetworkListResult{Items: []network.Summary{{Network: c.network.Network}}}, nil
}

// NetworkInspect returns network details for rollback preflight.
func (c driftNetworkClient) NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	return client.NetworkInspectResult{Network: c.network}, nil
}

// ContainerInspect supplies project container details for the drift snapshot.
func (c driftNetworkClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: container.InspectResponse{
		HostConfig: &container.HostConfig{RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}},
	}}, nil
}

// TestSelfNetworkDriftDefersFullProject ensures network recreation is deferred
// until the applier has a recoverable project snapshot.
func TestSelfNetworkDriftDefersFullProject(t *testing.T) {
	project := selfUpdateTestProject("self-stack", "app")
	svc := project.Services["app"]
	svc.Networks = map[string]*types.ServiceNetworkConfig{"backend": {}}
	project.Services["app"] = svc
	cfg := types.NetworkConfig{Name: "self-stack_backend", Labels: map[string]string{"generation": "new"}}
	project.Networks = types.Networks{"backend": cfg}

	hash, err := compose.NetworkHash(&cfg)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		liveName string
		hash     string
		drift    bool
	}{
		{name: "matching network", liveName: cfg.Name, hash: hash},
		{name: "changed configuration", liveName: cfg.Name, hash: "old", drift: true},
		{name: "renamed network", liveName: "self-stack_old", hash: hash, drift: true},
		{name: "network without hash is reused", liveName: cfg.Name},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := network.Inspect{
				ID: "network-id", Name: tc.liveName,
				Labels: map[string]string{
					api.ProjectLabel: "self-stack", api.NetworkLabel: "backend", api.ConfigHashLabel: tc.hash,
				},
			}
			docker := selfApplyTestCli{apiClient: driftNetworkClient{network: n}}

			drift, err := networkDrift(t.Context(), docker.Client(), project, svc)
			if err != nil || drift != tc.drift {
				t.Fatalf("networkDrift() = %v, %v; want %v", drift, err, tc.drift)
			}

			if !tc.drift {
				return
			}

			withSelfIdentity(t, "self-stack", "app")

			opts := SelfUpdateConfig()
			opts.Store = selfupdate.NewStore(t.TempDir())
			ConfigureSelfUpdate(opts)

			plan, err := prepareSelfUpdate(
				t.Context(), docker, project, &deploy.Config{}, &selfTarget{Project: project.Name, Service: "app"},
				[]string{"app", "worker"}, &SelfDeployInput{SourceType: "git"})
			if err != nil {
				t.Fatalf("prepareSelfUpdate(): %v", err)
			}

			if !plan.DeferToApplier || plan.Step == nil || !slices.Equal(plan.Services, []string{"worker"}) {
				t.Fatalf("network drift was not deferred to applier: deferred=%v services=%v", plan.DeferToApplier, plan.Services)
			}
		})
	}
}

// TestDeployComposePreflightBeforeSignalAndPull checks unsafe updates are
// rejected before pulling images or initiating a handover.
func TestDeployComposePreflightBeforeSignalAndPull(t *testing.T) {
	withSelfIdentity(t, "self-stack", "app")

	err := deployCompose(
		t.Context(),
		selfApplyTestCli{apiClient: driftNetworkClient{}},
		selfUpdateTestProject("self-stack", "app"),
		&deploy.Config{Name: "self-stack"},
		"", nil,
		[]SignalService{{ServiceName: "other", Signal: "KILL"}},
		nil, nil,
	)
	if !errors.Is(err, selfupdate.ErrUnsupported) {
		t.Fatalf("deployCompose() error = %v; want unsupported self-update before signaling or pulling", err)
	}
}
