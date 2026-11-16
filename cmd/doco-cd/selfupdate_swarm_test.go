package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/streams"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/common/types/clone"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

type swarmFinalizerClient struct {
	client.APIClient
	config       *swarm.Config
	service      swarm.Service
	tasks        map[string]swarm.Task
	claims       int
	removed      int
	containerErr error
}

func (c *swarmFinalizerClient) ConfigInspect(_ context.Context, _ string, _ client.ConfigInspectOptions) (client.ConfigInspectResult, error) {
	if c.config == nil {
		return client.ConfigInspectResult{}, errdefs.ErrNotFound
	}

	return client.ConfigInspectResult{Config: *clone.New(c.config)}, nil
}

func (c *swarmFinalizerClient) ConfigUpdate(_ context.Context, id string, opts client.ConfigUpdateOptions) (client.ConfigUpdateResult, error) {
	if c.config == nil || c.config.ID != id {
		return client.ConfigUpdateResult{}, errdefs.ErrNotFound
	}

	if c.config.Version != opts.Version {
		return client.ConfigUpdateResult{}, errdefs.ErrConflict
	}

	c.config.Spec = *clone.New(&opts.Spec)
	c.config.Version.Index += 7
	c.claims++

	return client.ConfigUpdateResult{}, nil
}

func (c *swarmFinalizerClient) ConfigRemove(_ context.Context, id string, _ client.ConfigRemoveOptions) (client.ConfigRemoveResult, error) {
	if c.config == nil || c.config.ID != id {
		return client.ConfigRemoveResult{}, errdefs.ErrNotFound
	}

	c.config = nil
	c.removed++

	return client.ConfigRemoveResult{}, nil
}

func (c *swarmFinalizerClient) ServiceInspect(_ context.Context, _ string, _ client.ServiceInspectOptions) (client.ServiceInspectResult, error) {
	return client.ServiceInspectResult{Service: *clone.New(&c.service)}, nil
}

func (c *swarmFinalizerClient) TaskInspect(_ context.Context, id string, _ client.TaskInspectOptions) (client.TaskInspectResult, error) {
	task, ok := c.tasks[id]
	if !ok {
		return client.TaskInspectResult{}, errdefs.ErrNotFound
	}

	return client.TaskInspectResult{Task: task}, nil
}

func (c *swarmFinalizerClient) TaskList(context.Context, client.TaskListOptions) (client.TaskListResult, error) {
	return client.TaskListResult{Items: []swarm.Task{c.tasks["new-task"]}}, nil
}

func (c *swarmFinalizerClient) NodeList(context.Context, client.NodeListOptions) (client.NodeListResult, error) {
	return client.NodeListResult{Items: []swarm.Node{{
		ID: "node-id", Spec: swarm.NodeSpec{Availability: swarm.NodeAvailabilityActive},
		Status: swarm.NodeStatus{State: swarm.NodeStateReady},
	}}}, nil
}

func (c *swarmFinalizerClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: container.InspectResponse{State: &container.State{
		Running: true, Health: &container.Health{Status: container.Healthy},
	}}}, c.containerErr
}

type swarmFinalizerCLI struct {
	command.Cli
	apiClient client.APIClient
}

func (c swarmFinalizerCLI) Client() client.APIClient { return c.apiClient }
func (c swarmFinalizerCLI) Out() *streams.Out        { return streams.NewOut(io.Discard) }
func (c swarmFinalizerCLI) Err() *streams.Out        { return streams.NewOut(io.Discard) }

func newSwarmFinalizerTest(t *testing.T, serverURL string) (*swarmFinalizerClient, selfupdate.Identity, selfupdate.SwarmHandover) {
	t.Helper()

	record := handedOverRecord(serverURL, selfupdate.StateHandover)
	record.Version = selfupdate.RecordVersion
	record.Strategy = selfupdate.StrategySwarm
	record.CreatedAt = time.Now()
	record.Deploy = selfupdate.DeployInfo{TimeoutSeconds: 30, Services: []string{"service-id"}}
	spec := swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "busybox:latest", Command: []string{"sleep", "600"}}}

	hash, err := selfupdate.SwarmTaskSpecHash(spec)
	if err != nil {
		t.Fatal(err)
	}

	handover := selfupdate.SwarmHandover{
		Record: record, ServiceID: "service-id", PredecessorTaskID: "old-task",
		TaskTemplateHash: hash, DesiredImage: "busybox:latest",
	}

	data, err := json.Marshal(handover)
	if err != nil {
		t.Fatal(err)
	}

	replicas := uint64(1)
	apiClient := &swarmFinalizerClient{
		config: &swarm.Config{ID: "config-id", Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: "doco-cd-self-update-service-id", Labels: map[string]string{
				selfupdate.SwarmHandoverLabel: record.ID, "cd.doco.self.swarm.service_id": handover.ServiceID,
				"cd.doco.self.swarm.state": string(selfupdate.StateHandover),
			}},
			Data: data,
		}},
		service: swarm.Service{ID: handover.ServiceID, Spec: swarm.ServiceSpec{
			Name: "self_app", TaskTemplate: spec, Labels: map[string]string{selfupdate.SwarmHandoverLabel: record.ID},
			Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
		}},
		tasks: map[string]swarm.Task{
			"old-task": {ID: "old-task", Status: swarm.TaskStatus{State: swarm.TaskStateShutdown}},
			"new-task": {
				ID: "new-task", NodeID: "node-id", ServiceID: handover.ServiceID, Spec: *clone.New(&spec),
				DesiredState: swarm.TaskStateRunning, Status: swarm.TaskStatus{State: swarm.TaskStateRunning},
			},
		},
	}
	apiClient.config.Version.Index = 10
	identity := selfupdate.Identity{OK: true, Swarm: true, SwarmServiceID: handover.ServiceID, SwarmTaskID: "new-task", ContainerID: "new"}

	pending, err := selfupdate.LoadSwarmHandover(t.Context(), apiClient, handover.ServiceID)
	if err != nil {
		t.Fatal(err)
	}

	return apiClient, identity, *pending
}

func TestSwarmFinalizerReportsSuccessAndFailure(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"success", "rollback", "timeout", "old task timeout"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			recorder := newCommitStatusRecorder(t)
			apiClient, identity, handover := newSwarmFinalizerTest(t, recorder.URL)
			want := commitstatus.StateSuccess

			switch name {
			case "rollback":
				apiClient.service.UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateStateRollbackCompleted}
				want = commitstatus.StateFailure
			case "timeout":
				handover.Record.CreatedAt = time.Now().Add(-time.Hour)
				want = commitstatus.StateFailure
			case "old task timeout":
				handover.Record.CreatedAt = time.Now().Add(-time.Hour)
				task := apiClient.tasks["new-task"]
				task.Spec.ContainerSpec.Image = "busybox:old"
				apiClient.tasks["new-task"] = task
				want = commitstatus.StateFailure
			}

			if err := finalizeSwarmSelfUpdate(t.Context(), logger.New(slog.LevelError), swarmFinalizerCLI{apiClient: apiClient},
				newSelfUpdateReporter(recorder.appConfig(), nil), identity, handover); err != nil {
				t.Fatal(err)
			}

			recorder.requireSingle(t, want)

			if apiClient.config != nil || apiClient.removed != 1 || apiClient.claims != 1 {
				t.Fatalf("handover cleanup = %+v", apiClient)
			}
		})
	}
}

func TestSwarmFinalizerWaitsForPredecessorAndIgnoresOldTask(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"predecessor still running", "old task"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			recorder := newCommitStatusRecorder(t)

			apiClient, identity, handover := newSwarmFinalizerTest(t, recorder.URL)
			if name == "predecessor still running" {
				apiClient.tasks["old-task"] = swarm.Task{Status: swarm.TaskStatus{State: swarm.TaskStateRunning}}
			} else {
				task := apiClient.tasks["new-task"]
				task.Spec.ContainerSpec.Image = "busybox:old"
				apiClient.tasks["new-task"] = task
			}

			if err := finalizeSwarmSelfUpdate(t.Context(), logger.New(slog.LevelError), swarmFinalizerCLI{apiClient: apiClient},
				newSelfUpdateReporter(recorder.appConfig(), nil), identity, handover); err != nil {
				t.Fatal(err)
			}

			if apiClient.claims != 0 || apiClient.removed != 0 || len(recorder.statuses()) != 0 {
				t.Fatal("unconfirmed handover was reported")
			}
		})
	}
}

func TestSwarmFinalizerRetainsAndRetriesFailedPost(t *testing.T) {
	t.Parallel()

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(failing.Close)

	apiClient, identity, handover := newSwarmFinalizerTest(t, failing.URL)
	if err := selfupdate.ClaimSwarmHandover(t.Context(), apiClient, handover, "new-task", nil); err != nil {
		t.Fatal(err)
	}

	pending, err := selfupdate.LoadSwarmHandover(t.Context(), apiClient, identity.SwarmServiceID)
	if err != nil {
		t.Fatal(err)
	}

	cfg := (&commitStatusRecorder{Server: failing}).appConfig()
	log := logger.New(slog.LevelError)

	cli := swarmFinalizerCLI{apiClient: apiClient}
	if err := finalizeSwarmSelfUpdate(t.Context(), log, cli, newSelfUpdateReporter(cfg, nil), identity, *pending); err == nil {
		t.Fatal("failed status post was acknowledged")
	}

	if apiClient.config == nil || apiClient.removed != 0 {
		t.Fatal("failed post removed the retry record")
	}

	recorder := newCommitStatusRecorder(t)

	cfg.GitScmApiUrl = recorder.appConfig().GitScmApiUrl
	if err := finalizeSwarmSelfUpdate(t.Context(), log, cli, newSelfUpdateReporter(cfg, nil), identity, *pending); err != nil {
		t.Fatal(err)
	}

	recorder.requireSingle(t, commitstatus.StateSuccess)

	if apiClient.config != nil || apiClient.claims != 1 {
		t.Fatal("retry did not reuse the persisted successful result")
	}
}

func TestSwarmFinalizerRecoversDeadOwner(t *testing.T) {
	t.Parallel()

	for _, alive := range []bool{false, true} {
		t.Run(map[bool]string{false: "dead owner", true: "live owner"}[alive], func(t *testing.T) {
			t.Parallel()

			recorder := newCommitStatusRecorder(t)

			apiClient, identity, handover := newSwarmFinalizerTest(t, recorder.URL)
			if err := selfupdate.ClaimSwarmHandover(t.Context(), apiClient, handover, "previous-finalizer", errors.New("rolled back")); err != nil {
				t.Fatal(err)
			}

			if alive {
				apiClient.tasks["previous-finalizer"] = swarm.Task{Status: swarm.TaskStatus{State: swarm.TaskStateRunning}}
			}

			pending, err := selfupdate.LoadSwarmHandover(t.Context(), apiClient, identity.SwarmServiceID)
			if err != nil {
				t.Fatal(err)
			}

			if err := finalizeSwarmSelfUpdate(t.Context(), logger.New(slog.LevelError), swarmFinalizerCLI{apiClient: apiClient},
				newSelfUpdateReporter(recorder.appConfig(), nil), identity, *pending); err != nil {
				t.Fatal(err)
			}

			if alive {
				if apiClient.claims != 1 || len(recorder.statuses()) != 0 || apiClient.config == nil {
					t.Fatal("reporting was stolen from a live task")
				}
			} else {
				recorder.requireSingle(t, commitstatus.StateFailure)

				if apiClient.claims != 2 || apiClient.config != nil {
					t.Fatal("dead reporting task was not recovered")
				}
			}
		})
	}
}

func TestSwarmFinalizerReportsOwnPersistedResult(t *testing.T) {
	t.Parallel()

	recorder := newCommitStatusRecorder(t)
	apiClient, identity, handover := newSwarmFinalizerTest(t, recorder.URL)
	identity.SwarmTaskID = "old-task"
	apiClient.tasks["old-task"] = swarm.Task{Status: swarm.TaskStatus{State: swarm.TaskStateRunning}}

	if err := selfupdate.ClaimSwarmHandover(t.Context(), apiClient, handover, identity.SwarmTaskID, nil); err != nil {
		t.Fatal(err)
	}

	pending, err := selfupdate.LoadSwarmHandover(t.Context(), apiClient, handover.ServiceID)
	if err != nil {
		t.Fatal(err)
	}

	if err := finalizeSwarmSelfUpdate(t.Context(), logger.New(slog.LevelError), swarmFinalizerCLI{apiClient: apiClient},
		newSelfUpdateReporter(recorder.appConfig(), nil), identity, *pending); err != nil {
		t.Fatal(err)
	}

	recorder.requireSingle(t, commitstatus.StateSuccess)

	if apiClient.config != nil {
		t.Fatal("the reporting task could not finalize its own saved result")
	}
}

func TestSwarmFinalizerPreservesShutdown(t *testing.T) {
	t.Parallel()

	recorder := newCommitStatusRecorder(t)
	apiClient, identity, handover := newSwarmFinalizerTest(t, recorder.URL)
	ctx, cancel := context.WithCancel(t.Context())
	apiClient.containerErr = context.Canceled

	cancel()

	if err := finalizeSwarmSelfUpdate(ctx, logger.New(slog.LevelError), swarmFinalizerCLI{apiClient: apiClient},
		newSelfUpdateReporter(recorder.appConfig(), nil), identity, handover); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown = %v, want cancellation", err)
	}

	if apiClient.config == nil || apiClient.claims != 0 || len(recorder.statuses()) != 0 {
		t.Fatal("shutdown resolved the pending deployment")
	}
}
