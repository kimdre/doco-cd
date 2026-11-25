package swarm

import (
	"context"
	"testing"
	"time"

	"github.com/docker/cli/cli/compose/convert"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker/options"
	"github.com/kimdre/doco-cd/internal/test"
)

// TestDeployServicesPreserveReplicasIntegration verifies that a redeploy keeps a
// replica count that was changed outside of doco-cd while the label is set, and
// that the Compose replica count applies again after the label is removed.
func TestDeployServicesPreserveReplicasIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping swarm preserve_replicas integration test in short mode")
	}

	dockerCli := getDockerCli(t)
	apiClient := dockerCli.Client()

	if !resolveTestSwarmMode(t, apiClient) {
		t.Skip("swarm mode is not enabled, skipping preserve_replicas integration test")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	namespace := convert.NewNamespace(test.ConvertTestName(t.Name()))
	serviceName := namespace.Scope("web")
	image := "busybox:latest"
	opts := &options.Deploy{ResolveImage: ResolveImageNever}

	t.Cleanup(func() {
		_, _ = apiClient.ServiceRemove(context.Background(), serviceName, client.ServiceRemoveOptions{})
	})

	deploy := func(labels map[string]string) {
		t.Helper()

		replicas := uint64(1)
		spec := swarmTypes.ServiceSpec{
			Name: serviceName,
			Labels: map[string]string{
				convert.LabelNamespace: namespace.Name(),
				convert.LabelImage:     image,
			},
			Mode: swarmTypes.ServiceMode{Replicated: &swarmTypes.ReplicatedService{Replicas: &replicas}},
			TaskTemplate: swarmTypes.TaskSpec{ContainerSpec: &swarmTypes.ContainerSpec{
				Image:   image,
				Command: []string{"sh", "-c", "sleep 600"},
				Labels:  labels,
			}},
		}

		if _, err := deployServices(ctx, dockerCli, map[string]swarmTypes.ServiceSpec{"web": spec}, namespace, opts); err != nil {
			t.Fatalf("deploy service %s: %v", serviceName, err)
		}
	}

	assertReplicas := func(want uint64) {
		t.Helper()

		result, err := apiClient.ServiceInspect(ctx, serviceName, client.ServiceInspectOptions{})
		if err != nil {
			t.Fatalf("inspect service %s: %v", serviceName, err)
		}

		replicated := result.Service.Spec.Mode.Replicated
		if replicated == nil || replicated.Replicas == nil || *replicated.Replicas != want {
			t.Fatalf("service %s replicas = %v, want %d", serviceName, replicated, want)
		}
	}

	preserve := map[string]string{preserveReplicasLabel: "true"}

	deploy(preserve)
	assertReplicas(1)

	if err := ScaleService(ctx, dockerCli, serviceName, 3, false, false); err != nil {
		t.Fatalf("scale service %s: %v", serviceName, err)
	}

	deploy(preserve)
	assertReplicas(3)

	deploy(nil)
	assertReplicas(1)
}
