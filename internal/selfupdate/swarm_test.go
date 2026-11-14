package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/lifecycle"
	"github.com/kimdre/doco-cd/internal/common/types/clone"
)

type swarmConfigClient struct {
	client.APIClient
	mu     sync.Mutex
	config *swarm.Config
}

func (c *swarmConfigClient) ConfigCreate(_ context.Context, opts client.ConfigCreateOptions) (client.ConfigCreateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.config != nil {
		return client.ConfigCreateResult{}, errdefs.ErrConflict
	}

	c.config = &swarm.Config{ID: "config-id", Spec: *clone.New(&opts.Spec)}
	c.config.Version.Index = 10

	return client.ConfigCreateResult{ID: c.config.ID}, nil
}

func (c *swarmConfigClient) ConfigInspect(_ context.Context, id string, _ client.ConfigInspectOptions) (client.ConfigInspectResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.config == nil || (id != c.config.ID && id != c.config.Spec.Name) {
		return client.ConfigInspectResult{}, errdefs.ErrNotFound
	}

	return client.ConfigInspectResult{Config: *clone.New(c.config)}, nil
}

func (c *swarmConfigClient) ConfigUpdate(_ context.Context, id string, opts client.ConfigUpdateOptions) (client.ConfigUpdateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.config == nil || c.config.ID != id {
		return client.ConfigUpdateResult{}, errdefs.ErrNotFound
	}

	if c.config.Version != opts.Version {
		return client.ConfigUpdateResult{}, errdefs.ErrConflict
	}

	if string(opts.Spec.Data) != string(c.config.Spec.Data) || opts.Spec.Name != c.config.Spec.Name {
		return client.ConfigUpdateResult{}, errors.New("only labels can change")
	}

	c.config.Spec = *clone.New(&opts.Spec)
	c.config.Version.Index += 7

	return client.ConfigUpdateResult{}, nil
}

func (c *swarmConfigClient) ConfigRemove(_ context.Context, id string, _ client.ConfigRemoveOptions) (client.ConfigRemoveResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.config == nil || c.config.ID != id {
		return client.ConfigRemoveResult{}, errdefs.ErrNotFound
	}

	c.config = nil

	return client.ConfigRemoveResult{}, nil
}

func testSwarmHandover(t *testing.T) (SwarmHandover, swarm.Service, swarm.Task) {
	t.Helper()

	spec := swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "busybox:latest", Command: []string{"sleep", "600"}}}

	hash, err := SwarmTaskSpecHash(spec)
	if err != nil {
		t.Fatal(err)
	}

	handover := SwarmHandover{
		Record:    Record{ID: "handover", Stack: "self", Deploy: DeployInfo{TimeoutSeconds: 60}},
		ServiceID: "service-id", PredecessorTaskID: "old-task", ServiceVersion: 5,
		TaskTemplateHash: hash, DesiredImage: spec.ContainerSpec.Image,
	}
	service := swarm.Service{ID: handover.ServiceID, Spec: swarm.ServiceSpec{
		Name: "self_app", Labels: map[string]string{SwarmHandoverLabel: handover.Record.ID}, TaskTemplate: spec,
	}}
	service.Version.Index = 10
	task := swarm.Task{
		ID: "new-task", ServiceID: handover.ServiceID, Spec: *clone.New(&spec),
		DesiredState: swarm.TaskStateRunning, Status: swarm.TaskStatus{State: swarm.TaskStateRunning},
	}

	return handover, service, task
}

func TestSwarmHandoverPersistence(t *testing.T) {
	t.Parallel()

	apiClient := &swarmConfigClient{}
	handover, _, _ := testSwarmHandover(t)
	// #nosec G101: dummy URL credentials verify that reporting metadata is sanitized.
	handover.Record.Source.CommitStatus = &CommitStatusInfo{
		SourceURL: "https://user:do-not-persist@example.com/owner/repo.git?token=do-not-persist",
		CommitSHA: "original-sha", Context: "original-context", Summary: "original-summary",
	}
	originalURL := handover.Record.Source.CommitStatus.SourceURL

	if err := CreateSwarmHandover(t.Context(), apiClient, &handover); err != nil {
		t.Fatal(err)
	}

	if handover.Config.Spec.Labels[swarmStackNamespaceLabel] != "" {
		t.Fatal("handover config can be pruned with the stack")
	}

	if strings.Contains(string(handover.Config.Spec.Data), "do-not-persist") {
		t.Fatal("URL credentials were stored")
	}

	if originalURL == handover.Record.Source.CommitStatus.SourceURL {
		t.Fatal("reporting URL was not sanitized")
	}

	current, err := LoadSwarmHandover(t.Context(), apiClient, handover.ServiceID)
	if err != nil {
		t.Fatal(err)
	}

	if current.Record.Strategy != StrategySwarm || current.Record.Source.CommitStatus.CommitSHA != "original-sha" ||
		current.Record.Source.CommitStatus.Context != "original-context" || current.Record.Source.CommitStatus.Summary != "original-summary" {
		t.Fatalf("reporting context changed: %+v", current.Record)
	}

	if err := CreateSwarmHandover(t.Context(), apiClient, &handover); !errdefs.IsConflict(err) {
		t.Fatalf("second handover = %v, want conflict", err)
	}

	cause := lifecycle.MarkTimedOut(errors.New("convergence timed out"))
	if err := ClaimSwarmHandover(t.Context(), apiClient, *current, "successor-task", cause); err != nil {
		t.Fatal(err)
	}

	if err := ClaimSwarmHandover(t.Context(), apiClient, *current, "another-task", nil); !errdefs.IsConflict(err) {
		t.Fatalf("stale claim = %v, want conflict", err)
	}

	claimed, err := LoadSwarmHandover(t.Context(), apiClient, handover.ServiceID)
	if err != nil {
		t.Fatal(err)
	}

	if claimed.OwnerTaskID != "successor-task" || claimed.Record.State != StateFinalising ||
		!claimed.Record.TimedOut || claimed.Record.Error != cause.Error() {
		t.Fatalf("claim was not durable: %+v", claimed)
	}

	if err := RemoveSwarmHandover(t.Context(), apiClient, *claimed); err != nil {
		t.Fatal(err)
	}

	if pending, err := LoadSwarmHandover(t.Context(), apiClient, handover.ServiceID); err != nil || pending != nil {
		t.Fatalf("removed handover = %v, %v", pending, err)
	}
}

func TestSwarmSuccessorCorrelation(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"success", "rollback", "paused", "superseded", "wrong task", "old image", "draining task", "changed spec", "resolved image", "changed pinned digest"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			handover, service, task := testSwarmHandover(t)
			wantUpdateErr := false
			wantMatch := name == "success" || name == "resolved image"

			switch name {
			case "rollback":
				service.UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateStateRollbackCompleted}
				wantUpdateErr = true
			case "paused":
				service.UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateStatePaused}
				wantUpdateErr = true
			case "superseded":
				service.Spec.Labels[SwarmHandoverLabel] = "another-handover"
				wantUpdateErr = true
			case "wrong task":
				task.ServiceID = "another-service"
			case "old image":
				task.Spec.ContainerSpec.Image = "busybox:old"
			case "draining task":
				task.DesiredState = swarm.TaskStateShutdown
			case "changed spec":
				service.Spec.TaskTemplate.ContainerSpec.Env = []string{"VALUE=changed"}
				wantUpdateErr = true
			case "resolved image":
				service.Spec.TaskTemplate.ContainerSpec.Image += "@sha256:" + strings.Repeat("a", 64)
				service.Spec.TaskTemplate.Placement = &swarm.Placement{Platforms: []swarm.Platform{{Architecture: "amd64", OS: "linux"}}}
				task.Spec = *clone.New(&service.Spec.TaskTemplate)
			case "changed pinned digest":
				handover.DesiredImage += "@sha256:" + strings.Repeat("a", 64)
				service.Spec.TaskTemplate.ContainerSpec.Image += "@sha256:" + strings.Repeat("b", 64)
				wantUpdateErr = true
			}

			if err := SwarmUpdateError(handover, service); (err != nil) != wantUpdateErr {
				t.Fatalf("update error = %v, want error %v", err, wantUpdateErr)
			}

			if !wantUpdateErr {
				matches, err := SwarmTaskMatches(handover, service, task)
				if err != nil || matches != wantMatch {
					t.Fatalf("task matches = %v, %v, want %v", matches, err, wantMatch)
				}
			}
		})
	}
}

func TestSwarmHandoverRejectsInvalidData(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"malformed", "wrong identity", "unknown state"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			apiClient := &swarmConfigClient{}

			handover, _, _ := testSwarmHandover(t)
			if err := CreateSwarmHandover(t.Context(), apiClient, &handover); err != nil {
				t.Fatal(err)
			}

			switch name {
			case "malformed":
				apiClient.config.Spec.Data = []byte("{")
			case "wrong identity":
				apiClient.config.Spec.Labels[SwarmHandoverLabel] = "wrong"
			case "unknown state":
				apiClient.config.Spec.Labels[swarmStateLabel] = "unexpected"
			}

			if _, err := LoadSwarmHandover(t.Context(), apiClient, handover.ServiceID); err == nil {
				t.Fatal("invalid handover was accepted")
			}
		})
	}
}

func TestSwarmHandoverRemovesExactConfig(t *testing.T) {
	t.Parallel()

	apiClient := &swarmConfigClient{}

	handover, _, _ := testSwarmHandover(t)
	if err := CreateSwarmHandover(t.Context(), apiClient, &handover); err != nil {
		t.Fatal(err)
	}

	apiClient.config.ID = "new-config-id"
	if err := RemoveSwarmHandover(t.Context(), apiClient, handover); err != nil {
		t.Fatal(err)
	}

	if apiClient.config == nil {
		t.Fatal("cleanup removed a later handover")
	}
}

func TestSwarmHandoverJSONContainsNoTaskConfiguration(t *testing.T) {
	t.Parallel()

	handover, service, _ := testSwarmHandover(t)
	service.Spec.TaskTemplate.ContainerSpec.Env = []string{"PRIVATE_VALUE=do-not-persist"}

	hash, err := SwarmTaskSpecHash(service.Spec.TaskTemplate)
	if err != nil {
		t.Fatal(err)
	}

	handover.TaskTemplateHash = hash
	handover.Record.CreatedAt = time.Now()

	data, err := json.Marshal(handover)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(data), "do-not-persist") || strings.Contains(string(data), "PRIVATE_VALUE") {
		t.Fatal("task environment was stored instead of its fingerprint")
	}
}
