package swarm

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// TestWaitOnServiceDetectsRollbackIntegration runs a real Swarm service update
// that fails and rolls back. It verifies waitOnService reports that rollback as
// an error instead of treating the deployment as successful.
func TestWaitOnServiceDetectsRollbackIntegration(t *testing.T) {
	requireSwarmRollbackIntegrationTestGate(t)

	dockerCli := getDockerCli(t)
	apiClient := dockerCli.Client()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	serviceName := fmt.Sprintf("doco-cd-rollback-%d", time.Now().UnixNano())
	replicas := uint64(1)

	createSpec := swarmTypes.ServiceSpec{
		Name: serviceName,
		Labels: map[string]string{
			"doco-cd.test": "rollback-detection",
		},
		TaskTemplate: swarmTypes.TaskSpec{
			ContainerSpec: &swarmTypes.ContainerSpec{
				Image:   "busybox:latest",
				Command: []string{"sh", "-c", "sleep 600"},
			},
		},
		Mode: swarmTypes.ServiceMode{
			Replicated: &swarmTypes.ReplicatedService{
				Replicas: &replicas,
			},
		},
		UpdateConfig: &swarmTypes.UpdateConfig{
			Parallelism:     1,
			Delay:           0,
			FailureAction:   swarmTypes.UpdateFailureActionRollback,
			Monitor:         15 * time.Second,
			MaxFailureRatio: 0,
			Order:           swarmTypes.UpdateOrderStopFirst,
		},
		RollbackConfig: &swarmTypes.UpdateConfig{
			Parallelism:     1,
			Delay:           0,
			FailureAction:   swarmTypes.UpdateFailureActionPause,
			Monitor:         15 * time.Second,
			MaxFailureRatio: 0,
			Order:           swarmTypes.UpdateOrderStopFirst,
		},
	}

	createRes, err := apiClient.ServiceCreate(ctx, client.ServiceCreateOptions{
		Spec:          createSpec,
		QueryRegistry: true,
	})
	if err != nil {
		t.Fatalf("failed to create test service %q: %v", serviceName, err)
	}

	t.Cleanup(func() {
		_, _ = apiClient.ServiceRemove(context.Background(), createRes.ID, client.ServiceRemoveOptions{})
	})

	if err = waitOnService(ctx, dockerCli, createRes.ID); err != nil {
		t.Fatalf("expected initial service rollout to succeed, got error: %v", err)
	}

	inspectRes, err := apiClient.ServiceInspect(ctx, createRes.ID, client.ServiceInspectOptions{})
	if err != nil {
		t.Fatalf("failed to inspect test service %q before update: %v", serviceName, err)
	}

	updateSpec := inspectRes.Service.Spec
	updateSpec.TaskTemplate.ForceUpdate++
	updateSpec.TaskTemplate.ContainerSpec.Image = "invalid.invalid/doco-cd/missing:latest"

	_, err = apiClient.ServiceUpdate(ctx, createRes.ID, client.ServiceUpdateOptions{
		Version: inspectRes.Service.Version,
		Spec:    updateSpec,
	})
	if err != nil {
		t.Fatalf("failed to update test service %q to trigger rollback: %v", serviceName, err)
	}

	err = waitOnService(ctx, dockerCli, createRes.ID)
	if err == nil {
		t.Fatalf("expected rollback detection error for service %q, got nil", serviceName)
	}

	if !strings.Contains(err.Error(), "rollback state") {
		t.Fatalf("expected rollback detection error, got: %v", err)
	}

	finalInspect, err := apiClient.ServiceInspect(ctx, createRes.ID, client.ServiceInspectOptions{})
	if err != nil {
		t.Fatalf("failed to inspect test service %q after rollback: %v", serviceName, err)
	}

	if finalInspect.Service.UpdateStatus == nil {
		t.Fatalf("expected update status to be present for service %q after rollback", serviceName)
	}

	if !isRollbackUpdateState(finalInspect.Service.UpdateStatus.State) {
		t.Fatalf("expected rollback update state, got %q", finalInspect.Service.UpdateStatus.State)
	}
}

func requireSwarmRollbackIntegrationTestGate(t *testing.T) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping swarm rollback integration test in short mode")
	}

	dockerClient := getDockerClient(t)
	if !resolveTestSwarmMode(t, dockerClient) {
		t.Skip("swarm mode is not enabled, skipping rollback integration test")
	}
}
