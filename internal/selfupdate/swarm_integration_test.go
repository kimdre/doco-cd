package selfupdate

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/docker/cli/cli/compose/convert"
	composeTypes "github.com/docker/cli/cli/compose/types"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/types/clone"
	"github.com/kimdre/doco-cd/internal/test"
)

func waitSwarmHandoverTask(ctx context.Context, t *testing.T, apiClient client.APIClient, serviceID, previousID string) swarm.Task {
	t.Helper()

	for {
		tasks, err := apiClient.TaskList(ctx, client.TaskListOptions{
			Filters: make(client.Filters).Add("service", serviceID).Add("desired-state", string(swarm.TaskStateRunning)),
		})
		if err != nil {
			t.Fatal(err)
		}

		for _, task := range tasks.Items {
			if task.ID != previousID && task.Status.State == swarm.TaskStateRunning {
				return task
			}
		}

		select {
		case <-ctx.Done():
			t.Fatalf("waiting for successor task: %v", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// TestSwarmHandoverDockerIntegration verifies real API defaults, versioned config claims,
// metadata-only updates, and durable recovery after a service rollback.
func TestSwarmHandoverDockerIntegration(t *testing.T) {
	if os.Getenv("DOCO_CD_RUN_DOCKER_INTEGRATION_TESTS") != "1" {
		t.Skip("Docker integration tests are not enabled")
	}

	apiClient, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := apiClient.Close(); err != nil {
			t.Logf("close Docker client: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	info, err := apiClient.Info(ctx, client.InfoOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if !info.Info.Swarm.ControlAvailable {
		t.Skip("Swarm confirmation integration requires a Swarm manager")
	}

	stack := test.ConvertTestName(t.Name())

	spec, err := convert.Service(convert.NewNamespace(stack), composeTypes.ServiceConfig{
		Name: "app", Image: "busybox:latest", Command: []string{"sleep", "600"},
	}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	spec.TaskTemplate.Networks = nil

	created, err := apiClient.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if _, err := apiClient.ServiceRemove(cleanupCtx, created.ID, client.ServiceRemoveOptions{}); err != nil {
			t.Logf("remove test service: %v", err)
		}
	})

	oldTask := waitSwarmHandoverTask(ctx, t, apiClient, created.ID, "")

	current, err := apiClient.ServiceInspect(ctx, created.ID, client.ServiceInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	metadataOnly := clone.New(&current.Service.Spec)

	metadataOnly.Labels[SwarmHandoverLabel] = "metadata-only"
	if _, err := apiClient.ServiceUpdate(ctx, created.ID, client.ServiceUpdateOptions{Spec: *metadataOnly, Version: current.Service.Version}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Second)

	stillRunning := waitSwarmHandoverTask(ctx, t, apiClient, created.ID, "")
	if stillRunning.ID != oldTask.ID {
		t.Fatal("service-level handover metadata replaced the task")
	}

	current, err = apiClient.ServiceInspect(ctx, created.ID, client.ServiceInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	desired := clone.New(&spec)
	desired.TaskTemplate.ContainerSpec.Args = []string{"sleep", "500"}

	hash, err := SwarmTaskSpecHash(desired.TaskTemplate)
	if err != nil {
		t.Fatal(err)
	}

	handover := SwarmHandover{
		Record:    Record{ID: "integration-handover", Stack: stack},
		ServiceID: created.ID, PredecessorTaskID: oldTask.ID, ServiceVersion: current.Service.Version.Index,
		TaskTemplateHash: hash, DesiredImage: desired.TaskTemplate.ContainerSpec.Image,
	}
	if err := CreateSwarmHandover(ctx, apiClient, &handover); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := RemoveSwarmHandover(cleanupCtx, apiClient, handover); err != nil {
			t.Logf("remove test handover config: %v", err)
		}
	})

	desired.Labels[SwarmHandoverLabel] = handover.Record.ID
	if _, err := apiClient.ServiceUpdate(ctx, created.ID, client.ServiceUpdateOptions{Spec: *desired, Version: current.Service.Version}); err != nil {
		t.Fatal(err)
	}

	newTask := waitSwarmHandoverTask(ctx, t, apiClient, created.ID, oldTask.ID)

	current, err = apiClient.ServiceInspect(ctx, created.ID, client.ServiceInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := SwarmUpdateError(handover, current.Service); err != nil {
		beforeJSON, _ := json.Marshal(desired.TaskTemplate)
		afterJSON, _ := json.Marshal(current.Service.Spec.TaskTemplate)
		t.Fatalf("desired service was not recognized: %v\nbefore: %s\nafter: %s", err, beforeJSON, afterJSON)
	}

	if matches, err := SwarmTaskMatches(handover, current.Service, newTask); err != nil || !matches {
		t.Fatalf("successor does not match: %v, %v", matches, err)
	}

	pending, err := LoadSwarmHandover(ctx, apiClient, created.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := ClaimSwarmHandover(ctx, apiClient, *pending, newTask.ID, nil); err != nil {
		t.Fatal(err)
	}

	claimed, err := LoadSwarmHandover(ctx, apiClient, created.ID)
	if err != nil || claimed.OwnerTaskID != newTask.ID || claimed.Record.State != StateFinalising {
		t.Fatalf("config annotation update was not durable: %+v, %v", claimed, err)
	}

	if _, err := apiClient.ServiceUpdate(ctx, created.ID, client.ServiceUpdateOptions{
		Spec: current.Service.Spec, Version: current.Service.Version, Rollback: "previous",
	}); err != nil {
		t.Fatal(err)
	}

	for {
		current, err = apiClient.ServiceInspect(ctx, created.ID, client.ServiceInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}

		if current.Service.UpdateStatus != nil && current.Service.UpdateStatus.State == swarm.UpdateStateRollbackCompleted {
			break
		}

		select {
		case <-ctx.Done():
			t.Fatalf("waiting for rollback: %v", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}

	if err := SwarmUpdateError(handover, current.Service); err == nil {
		t.Fatal("rolled-back service was confirmed as the desired update")
	}

	restored := waitSwarmHandoverTask(ctx, t, apiClient, created.ID, newTask.ID)

	previousHash, err := SwarmTaskSpecHash(oldTask.Spec)
	if err != nil {
		t.Fatal(err)
	}

	restoredHash, err := SwarmTaskSpecHash(restored.Spec)
	if err != nil || restoredHash != previousHash {
		t.Fatalf("rollback did not restore the original task specification: %s, %v", restoredHash, err)
	}

	if pending, err := LoadSwarmHandover(ctx, apiClient, created.ID); err != nil || pending == nil {
		t.Fatalf("rollback lost the reporting config: %+v, %v", pending, err)
	}
}
