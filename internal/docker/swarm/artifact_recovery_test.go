package swarm

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/docker/cli/cli/compose/convert"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker/options"
)

type artifactRecoveryClient struct {
	serviceIdentityClient
	updates []client.ServiceUpdateOptions
	creates []client.ServiceCreateOptions
}

func (c *artifactRecoveryClient) ServiceUpdate(_ context.Context, _ string, opts client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
	c.updates = append(c.updates, opts)
	if c.updateErr == nil {
		c.services[0].Spec = opts.Spec
		c.services[0].Version.Index++
	}

	return client.ServiceUpdateResult{}, c.updateErr
}

func (c *artifactRecoveryClient) ServiceCreate(_ context.Context, opts client.ServiceCreateOptions) (client.ServiceCreateResult, error) {
	c.creates = append(c.creates, opts)

	return client.ServiceCreateResult{ID: "new-service"}, c.createErr
}

func TestDeployServicesArtifactRecovery(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		mode     swarmTypes.ServiceMode
		force    []string
		existing bool
		want     uint64
	}{
		{name: "unchanged service keeps tasks", existing: true, want: 7},
		{name: "stale service replaces tasks", existing: true, force: []string{"api"}, want: 8},
		{name: "other stale service leaves tasks unchanged", existing: true, force: []string{"worker"}, want: 7},
		{name: "replicated job does not rerun", existing: true, force: []string{"api"}, mode: swarmTypes.ServiceMode{ReplicatedJob: &swarmTypes.ReplicatedJob{}}, want: 7},
		{name: "global job does not rerun", existing: true, force: []string{"api"}, mode: swarmTypes.ServiceMode{GlobalJob: &swarmTypes.GlobalJob{}}, want: 7},
		{name: "new service needs no forced update", force: []string{"api"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := swarmTypes.ServiceSpec{
				Name: "stack_api",
				Mode: tc.mode,
				TaskTemplate: swarmTypes.TaskSpec{
					ContainerSpec: &swarmTypes.ContainerSpec{Image: "example:latest"},
				},
			}
			apiClient := &artifactRecoveryClient{}

			if tc.existing {
				existing := spec
				existing.TaskTemplate.ForceUpdate = 7
				apiClient.services = []swarmTypes.Service{{ID: "api-id", Spec: existing}}
			}

			opts := &options.Deploy{ResolveImage: ResolveImageNever, ForceUpdateServices: tc.force}
			cli := testServiceIdentityCLI(apiClient, io.Discard, io.Discard)

			_, err := deployServices(t.Context(), cli, map[string]swarmTypes.ServiceSpec{"api": spec}, convert.NewNamespace("stack"), opts)
			if err != nil {
				t.Fatal(err)
			}

			var actual swarmTypes.ServiceSpec

			if tc.existing {
				if len(apiClient.updates) != 1 {
					t.Fatalf("updates = %d, want 1", len(apiClient.updates))
				}

				actual = apiClient.updates[0].Spec
			} else {
				if len(apiClient.creates) != 1 {
					t.Fatalf("creates = %d, want 1", len(apiClient.creates))
				}

				actual = apiClient.creates[0].Spec
			}

			if actual.TaskTemplate.ForceUpdate != tc.want {
				t.Fatalf("ForceUpdate = %d, want %d", actual.TaskTemplate.ForceUpdate, tc.want)
			}
		})
	}
}

func TestDeployServicesArtifactRecoveryFailurePreservesRetry(t *testing.T) {
	t.Parallel()

	updateErr := errors.New("service update rejected")
	apiClient := &artifactRecoveryClient{
		updateErr: updateErr,
		services: []swarmTypes.Service{{
			ID: "api-id",
			Spec: swarmTypes.ServiceSpec{
				Name:   "stack_api",
				Labels: map[string]string{"cd.doco.deployment.timestamp": "before-replacement"},
				TaskTemplate: swarmTypes.TaskSpec{
					ForceUpdate:   7,
					ContainerSpec: &swarmTypes.ContainerSpec{Image: "example:latest"},
				},
			},
		}},
	}
	newSpec := apiClient.services[0].Spec
	newSpec.Labels = map[string]string{"cd.doco.deployment.timestamp": "after-replacement"}
	services := map[string]swarmTypes.ServiceSpec{"api": newSpec}
	opts := &options.Deploy{ResolveImage: ResolveImageNever, ForceUpdateServices: []string{"api"}}
	cli := testServiceIdentityCLI(apiClient, io.Discard, io.Discard)

	if _, err := deployServices(t.Context(), cli, services, convert.NewNamespace("stack"), opts); !errors.Is(err, updateErr) {
		t.Fatalf("failed recovery error = %v, want %v", err, updateErr)
	}

	if got := apiClient.services[0].Spec.Labels["cd.doco.deployment.timestamp"]; got != "before-replacement" {
		t.Fatalf("failed update changed recovery timestamp to %q", got)
	}

	apiClient.updateErr = nil

	if _, err := deployServices(t.Context(), cli, services, convert.NewNamespace("stack"), opts); err != nil {
		t.Fatal(err)
	}

	if got := apiClient.services[0].Spec.TaskTemplate.ForceUpdate; got != 8 {
		t.Fatalf("retry ForceUpdate = %d, want 8", got)
	}

	if got := apiClient.services[0].Spec.Labels["cd.doco.deployment.timestamp"]; got != "after-replacement" {
		t.Fatalf("successful recovery timestamp = %q", got)
	}
}
