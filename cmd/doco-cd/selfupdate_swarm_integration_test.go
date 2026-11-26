package main

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/common/types/clone"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/selfupdate"
	"github.com/kimdre/doco-cd/internal/test"
)

func waitSwarmFinalizerTask(ctx context.Context, t *testing.T, apiClient client.APIClient, serviceID, previousID string) swarm.Task {
	t.Helper()

	for {
		result, err := apiClient.TaskList(ctx, client.TaskListOptions{
			Filters: make(client.Filters).Add("service", serviceID).Add("desired-state", string(swarm.TaskStateRunning)),
		})
		if err != nil {
			t.Fatal(err)
		}

		for _, task := range result.Items {
			if task.ID != previousID && task.Status.State == swarm.TaskStateRunning {
				return task
			}
		}

		select {
		case <-ctx.Done():
			t.Fatalf("wait for Swarm reporting task: %v", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func TestSwarmFinalizerDockerIntegration(t *testing.T) {
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
	replicas := uint64(1)
	spec := swarm.ServiceSpec{
		Name: stack + "_app",
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image: "busybox:latest", Command: []string{"sleep", "600"},
			Healthcheck: &container.HealthConfig{Test: []string{"CMD", "true"}, Interval: time.Second},
		}},
	}

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

	oldTask := waitSwarmFinalizerTask(ctx, t, apiClient, created.ID, "")

	current, err := apiClient.ServiceInspect(ctx, created.ID, client.ServiceInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	desired := clone.New(&current.Service.Spec)
	desired.TaskTemplate.ForceUpdate++

	hash, err := selfupdate.SwarmTaskSpecHash(desired.TaskTemplate)
	if err != nil {
		t.Fatal(err)
	}

	recorder := newCommitStatusRecorder(t)
	record := handedOverRecord(recorder.URL, selfupdate.StateHandover)
	record.Stack = stack
	record.Deploy = selfupdate.DeployInfo{TimeoutSeconds: 60, Services: []string{created.ID}}

	handover := selfupdate.SwarmHandover{
		Record: record, ServiceID: created.ID, PredecessorTaskID: oldTask.ID,
		ServiceVersion: current.Service.Version.Index, TaskTemplateHash: hash,
		DesiredImage: desired.TaskTemplate.ContainerSpec.Image,
	}
	if err := selfupdate.CreateSwarmHandover(ctx, apiClient, &handover); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := selfupdate.RemoveSwarmHandover(cleanupCtx, apiClient, handover); err != nil {
			t.Logf("remove test handover: %v", err)
		}
	})

	desired.Labels = map[string]string{selfupdate.SwarmHandoverLabel: record.ID}
	if _, err := apiClient.ServiceUpdate(ctx, created.ID, client.ServiceUpdateOptions{Version: current.Service.Version, Spec: *desired}); err != nil {
		t.Fatal(err)
	}

	newTask := waitSwarmFinalizerTask(ctx, t, apiClient, created.ID, oldTask.ID)
	identity := selfupdate.Identity{
		OK: true, Swarm: true, SwarmServiceID: created.ID, SwarmTaskID: newTask.ID,
		ContainerID: newTask.Status.ContainerStatus.ContainerID,
	}
	log := logger.New(slog.LevelError)

	for {
		pending, err := selfupdate.LoadSwarmHandover(ctx, apiClient, created.ID)
		if err != nil {
			t.Fatal(err)
		}

		if pending == nil {
			break
		}

		if err := finalizeSwarmSelfUpdate(ctx, log, swarmFinalizerCLI{apiClient: apiClient},
			newSelfUpdateReporter(recorder.appConfig(), nil), identity, *pending); err != nil {
			t.Fatal(err)
		}

		select {
		case <-ctx.Done():
			t.Fatalf("wait for final reporting: %v", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}

	recorder.requireSingle(t, commitstatus.StateSuccess)
}
