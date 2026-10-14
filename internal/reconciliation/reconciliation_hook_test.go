package reconciliation

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/notification"
)

type hookEventClient struct {
	client.APIClient
	inspections    int
	restarts       []string
	containerLists int
	serviceLists   int
}

func (c *hookEventClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	c.inspections++

	return client.ContainerInspectResult{}, errdefs.ErrNotFound
}

func (c *hookEventClient) ContainerRestart(_ context.Context, containerID string, _ client.ContainerRestartOptions) (client.ContainerRestartResult, error) {
	c.restarts = append(c.restarts, containerID)

	return client.ContainerRestartResult{}, nil
}

func (c *hookEventClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	c.containerLists++

	return client.ContainerListResult{}, nil
}

func (c *hookEventClient) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	c.serviceLists++

	return client.ServiceListResult{}, nil
}

func TestHandleEventLifecycleHooks(t *testing.T) {
	t.Parallel()

	for _, label := range []struct {
		name    string
		present bool
		value   string
	}{
		{name: "absent"},
		{name: "empty", present: true},
		{name: "pre_start", present: true, value: "pre_start"},
		{name: "post_start", present: true, value: "post_start"},
		{name: "pre_stop", present: true, value: "pre_stop"},
		{name: "other nonempty value", present: true, value: "custom:0"},
		{name: "whitespace", present: true, value: " "},
	} {
		for _, trigger := range []struct {
			name     string
			action   events.Action
			exitCode string
			swarm    bool
			restart  bool
		}{
			{name: "successful completion", action: "die", exitCode: "0"},
			{name: "failed completion", action: "die", exitCode: "1"},
			{name: "removal", action: "destroy"},
			{name: "stop", action: "stop", restart: true},
			{name: "kill", action: "kill", restart: true},
			{name: "oom", action: "oom", restart: true},
			{name: "unhealthy", action: "health_status: unhealthy", restart: true},
			{name: "swarm removal", action: "remove", swarm: true},
			{name: "swarm update", action: "update", swarm: true},
		} {
			t.Run(label.name+"/"+trigger.name, func(t *testing.T) {
				t.Parallel()

				var output bytes.Buffer

				log := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
				apiClient := &hookEventClient{}
				cli := swarmEventNameCLI{client: apiClient}
				contexts := docker.NewContextRegistry(nil, docker.ContextRegistryOptions{})

				t.Cleanup(func() { _ = contexts.Close() })

				manager := &Manager{
					dockerCli:      cli,
					contexts:       contexts,
					runtimeQueries: dockerRuntimeQueries{},
					notifier:       newTestNotifier(t),
					deployments:    deploymentTracker{stacks: make(map[string]int)},
				}
				dc := deployConfig.New("stack-a", "main")
				dc.Reconciliation.Enabled = true
				dc.Reconciliation.Events = []string{normalizeReconciliationEventAction(string(trigger.action))}
				req := DeployRequest{
					Logger:        log,
					Metadata:      notification.Metadata{Repository: t.Name()},
					DeployConfigs: []*deployConfig.Config{dc},
				}
				j := newJob(manager, req, getDeployConfigGroupByEvent(req.DeployConfigs))
				j.contextCLIs = map[string]contextCLIEntry{"": {cli: cli, swarmMode: trigger.swarm}}

				event := events.Message{
					Type:   events.ContainerEventType,
					Action: trigger.action,
					Actor: events.Actor{
						ID: "hook-or-workload",
						Attributes: map[string]string{
							"name":                              "stack-a_web",
							api.ProjectLabel:                    "stack-a",
							api.ServiceLabel:                    "web",
							docker.DocoCDLabels.Deployment.Name: "stack-a",
							"exitCode":                          trigger.exitCode,
						},
					},
				}
				if label.present {
					event.Actor.Attributes[api.HookLabel] = label.value
				}

				if trigger.swarm {
					event.Type = events.ServiceEventType
				}

				j.handleEvent(t.Context(), log, event, "", trigger.swarm)

				ignored := label.value != "" && !trigger.swarm
				if ignored {
					if apiClient.inspections != 0 || len(apiClient.restarts) != 0 || apiClient.containerLists != 0 || apiClient.serviceLists != 0 {
						t.Fatalf("hook event triggered reconciliation: inspections=%d, restarts=%v, container lists=%d, service lists=%d",
							apiClient.inspections, apiClient.restarts, apiClient.containerLists, apiClient.serviceLists)
					}

					if !strings.Contains(output.String(), "level=DEBUG") || !strings.Contains(output.String(), "skipping reconciliation for lifecycle-hook runner") {
						t.Fatalf("expected lifecycle-hook skip debug log, got %q", output.String())
					}

					return
				}

				if trigger.restart {
					if len(apiClient.restarts) != 1 || apiClient.restarts[0] != event.Actor.ID {
						t.Fatalf("expected workload container restart, got %v; logs: %s", apiClient.restarts, output.String())
					}
				} else {
					// The fixture has no deployment context, but must reach the redeploy
					// path and its cleanup query for ordinary containers and Swarm services.
					if apiClient.inspections != 1 || apiClient.containerLists+apiClient.serviceLists == 0 {
						t.Fatalf("expected workload redeploy, got inspections=%d, container lists=%d, service lists=%d; logs: %s",
							apiClient.inspections, apiClient.containerLists, apiClient.serviceLists, output.String())
					}
				}
			})
		}
	}
}
