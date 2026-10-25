package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	contextstore "github.com/docker/cli/cli/context/store"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	swarmInternal "github.com/kimdre/doco-cd/internal/docker/swarm"
)

func TestServiceStatesFromSwarm(t *testing.T) {
	t.Parallel()

	labels := map[string]string{swarmInternal.StackNamespaceLabel: "web"}
	service := func(name string, mode swarm.ServiceMode, l map[string]string) swarm.Service {
		return swarm.Service{Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: name, Labels: l}, Mode: mode}}
	}

	replicated := service("web_app", swarm.ServiceMode{Replicated: &swarm.ReplicatedService{}}, labels)
	replicated.ServiceStatus = &swarm.ServiceStatus{DesiredTasks: 3, RunningTasks: 2}
	replicated.UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateStateRollbackPaused}

	// Restart-mode scheduled jobs keep their replica count after a run, but
	// their tasks stop. They must not look like a degraded service.
	scheduled := service("web_backup", swarm.ServiceMode{Replicated: &swarm.ReplicatedService{}}, labels)
	scheduled.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{
		Labels: map[string]string{DocoCDJobLabels.JobEnabled: "true"},
	}
	scheduled.ServiceStatus = &swarm.ServiceStatus{DesiredTasks: 1, RunningTasks: 0}

	// Job config labels on the service spec are ignored, like in the scheduler.
	specOnly := service("web_worker", swarm.ServiceMode{Replicated: &swarm.ReplicatedService{}},
		map[string]string{swarmInternal.StackNamespaceLabel: "web", DocoCDJobLabels.JobEnabled: "true"})

	services := []swarm.Service{
		replicated,
		service("web_job", swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{}}, labels),
		service("web_oneoff", swarm.ServiceMode{Replicated: &swarm.ReplicatedService{}},
			map[string]string{swarmInternal.StackNamespaceLabel: "web", DocoCDJobLabels.JobEphemeral: "true"}),
		service("nostack", swarm.ServiceMode{Replicated: &swarm.ReplicatedService{}}, nil),
		scheduled,
		specOnly,
	}

	got := serviceStatesFromSwarm("default", services)
	if len(got) != 2 {
		t.Fatalf("got %d states, want 2: %+v", len(got), got)
	}

	if got[1].Service != "worker" {
		t.Errorf("got service %q, want %q", got[1].Service, "worker")
	}

	want := "default/web/app 3/2 rollback_paused"
	have := fmt.Sprintf("%s/%s/%s %d/%d %s", got[0].Context, got[0].Stack, got[0].Service,
		got[0].DesiredTasks, got[0].RunningTasks, got[0].UpdateState)

	if have != want {
		t.Errorf("got %q, want %q", have, want)
	}
}

type serviceStateTestClient struct {
	client.APIClient
	services []swarm.Service
}

func (c *serviceStateTestClient) ServiceList(_ context.Context, opts client.ServiceListOptions) (client.ServiceListResult, error) {
	if !opts.Status {
		return client.ServiceListResult{}, errors.New("service list without status")
	}

	return client.ServiceListResult{Items: c.services}, nil
}

func TestNewServiceStateSource(t *testing.T) {
	t.Parallel()

	stackService := func(name string, desired, running uint64) swarm.Service {
		return swarm.Service{
			Spec: swarm.ServiceSpec{
				Annotations: swarm.Annotations{Name: name, Labels: map[string]string{swarmInternal.StackNamespaceLabel: "web"}},
				Mode:        swarm.ServiceMode{Replicated: &swarm.ReplicatedService{}},
			},
			ServiceStatus: &swarm.ServiceStatus{DesiredTasks: desired, RunningTasks: running},
		}
	}

	defaultClient := &serviceStateTestClient{services: []swarm.Service{stackService("web_app", 2, 2)}}
	remoteClient := &serviceStateTestClient{services: []swarm.Service{stackService("web_api", 3, 1)}}
	slowClient := &serviceStateTestClient{}
	standaloneClient := &serviceStateTestClient{services: []swarm.Service{stackService("web_db", 1, 1)}}

	clis := map[string]command.Cli{
		"remote":     contextRegistryTestCli{apiClient: remoteClient},
		"slow":       contextRegistryTestCli{apiClient: slowClient},
		"standalone": contextRegistryTestCli{apiClient: standaloneClient},
	}

	registry := NewContextRegistry(contextRegistryTestCli{apiClient: defaultClient}, ContextRegistryOptions{Quiet: true, SwarmFeatures: true})
	registry.listContexts = func() ([]contextstore.Metadata, error) {
		return []contextstore.Metadata{{Name: "slow"}, {Name: "standalone"}, {Name: "remote"}}, nil
	}
	registry.createCli = func(_ bool, name string) (command.Cli, error) {
		return clis[name], nil
	}
	registry.resolveSwarm = func(ctx context.Context, apiClient client.APIClient) (bool, error) {
		switch apiClient {
		case slowClient:
			<-ctx.Done()

			return false, ctx.Err()
		case standaloneClient:
			return false, nil
		default:
			return true, nil
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	got := NewServiceStateSource(registry, slog.New(slog.DiscardHandler))(ctx)

	have := make([]string, 0, len(got))
	for _, s := range got {
		have = append(have, fmt.Sprintf("%s/%s/%s %d/%d", s.Context, s.Stack, s.Service, s.DesiredTasks, s.RunningTasks))
	}

	want := []string{"default/web/app 2/2", "remote/web/api 3/1"}
	if fmt.Sprint(have) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", have, want)
	}
}
