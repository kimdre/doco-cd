package reconciliation

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/notification"
)

// restartHookClient records container restarts and hook exec processes in one call log.
// A hook command fails if it is in failingCommands.
type restartHookClient struct {
	client.APIClient

	mu              sync.Mutex
	calls           []string
	execs           map[string]string
	failingCommands []string
}

func (c *restartHookClient) record(call string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls = append(c.calls, call)
}

func (c *restartHookClient) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.calls)
}

func (c *restartHookClient) ContainerRestart(_ context.Context, containerID string, _ client.ContainerRestartOptions) (client.ContainerRestartResult, error) {
	c.record("restart " + containerID)

	return client.ContainerRestartResult{}, nil
}

func (c *restartHookClient) ExecCreate(_ context.Context, containerID string, options client.ExecCreateOptions) (client.ExecCreateResult, error) {
	cmd := strings.Join(options.Cmd, " ")
	c.record("exec " + containerID + " " + cmd)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.execs == nil {
		c.execs = make(map[string]string)
	}

	id := "exec-" + cmd
	c.execs[id] = cmd

	return client.ExecCreateResult{ID: id}, nil
}

func (c *restartHookClient) ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error) {
	// The hook writes no output. The closed peer ends the output stream at once.
	local, peer := net.Pipe()
	_ = peer.Close()

	return client.ExecAttachResult{HijackedResponse: client.NewHijackedResponse(local, "")}, nil
}

func (c *restartHookClient) ExecInspect(_ context.Context, execID string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if slices.Contains(c.failingCommands, c.execs[execID]) {
		return client.ExecInspectResult{ExitCode: 1}, nil
	}

	return client.ExecInspectResult{}, nil
}

type recordedNotification struct {
	level notification.Level
	title string
}

type recordingNotifier struct {
	mu   sync.Mutex
	sent []recordedNotification
}

func (n *recordingNotifier) Send(level notification.Level, title, _ string, _ notification.Metadata, _ ...notification.SendOption) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.sent = append(n.sent, recordedNotification{level: level, title: title})

	return nil
}

func TestRestartContainerRunsComposeLifecycleHooks(t *testing.T) {
	t.Parallel()

	hookService := types.ServiceConfig{
		Name:      "web",
		PostStart: []types.ServiceHook{{Command: types.ShellCommand{"post_start"}}},
		PreStop:   []types.ServiceHook{{Command: types.ShellCommand{"pre_stop"}}},
	}

	for _, tc := range []struct {
		name              string
		service           types.ServiceConfig
		loaded            bool
		swarm             bool
		hooksLabel        *string
		failingCommands   []string
		wantCalls         []string
		wantLoads         int
		wantNotifications []recordedNotification
		wantLog           string
	}{
		{
			name:              "runs pre_stop and post_start around the restart",
			service:           hookService,
			loaded:            true,
			wantCalls:         []string{"exec web-1 pre_stop", "restart web-1", "exec web-1 post_start"},
			wantLoads:         1,
			wantNotifications: []recordedNotification{{level: notification.Success, title: "Container restarted"}},
		},
		{
			name:              "service without hooks",
			service:           types.ServiceConfig{Name: "web"},
			loaded:            true,
			wantCalls:         []string{"restart web-1"},
			wantLoads:         1,
			wantNotifications: []recordedNotification{{level: notification.Success, title: "Container restarted"}},
		},
		{
			name:              "unavailable service definition",
			service:           hookService,
			wantCalls:         []string{"restart web-1"},
			wantLoads:         1,
			wantNotifications: []recordedNotification{{level: notification.Success, title: "Container restarted"}},
		},
		{
			name:              "label without restart hooks skips the load",
			service:           hookService,
			loaded:            true,
			hooksLabel:        new("pre_start"),
			wantCalls:         []string{"restart web-1"},
			wantNotifications: []recordedNotification{{level: notification.Success, title: "Container restarted"}},
		},
		{
			name:              "label with restart hooks loads the definition",
			service:           hookService,
			loaded:            true,
			hooksLabel:        new("pre_start,pre_stop"),
			wantCalls:         []string{"exec web-1 pre_stop", "restart web-1", "exec web-1 post_start"},
			wantLoads:         1,
			wantNotifications: []recordedNotification{{level: notification.Success, title: "Container restarted"}},
		},
		{
			name:              "swarm mode skips hooks",
			service:           hookService,
			loaded:            true,
			swarm:             true,
			wantCalls:         []string{"restart web-1"},
			wantNotifications: []recordedNotification{{level: notification.Success, title: "Service restarted"}},
		},
		{
			name:              "failed pre_stop hook still restarts",
			service:           hookService,
			loaded:            true,
			failingCommands:   []string{"pre_stop"},
			wantCalls:         []string{"exec web-1 pre_stop", "restart web-1", "exec web-1 post_start"},
			wantLoads:         1,
			wantNotifications: []recordedNotification{{level: notification.Success, title: "Container restarted"}},
			wantLog:           "pre_stop hook failed, restarting the container anyway",
		},
		{
			name:              "failed post_start hook sends a warning",
			service:           hookService,
			loaded:            true,
			failingCommands:   []string{"post_start"},
			wantCalls:         []string{"exec web-1 pre_stop", "restart web-1", "exec web-1 post_start"},
			wantLoads:         1,
			wantNotifications: []recordedNotification{{level: notification.Warning, title: "Container restarted, post_start hook failed"}},
			wantLog:           "post_start hook failed after the container restart",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var output bytes.Buffer

			log := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
			apiClient := &restartHookClient{failingCommands: tc.failingCommands}
			cli := swarmEventNameCLI{client: apiClient}
			notifier := &recordingNotifier{}

			var loads int

			manager := &Manager{
				notifier:    notifier,
				deployments: deploymentTracker{stacks: make(map[string]int)},
				loadHookService: func(_ context.Context, _ *slog.Logger, _ command.Cli, labels map[string]string) (types.ServiceConfig, bool) {
					loads++

					if labels[api.ServiceLabel] != "web" {
						t.Errorf("hook loader got labels %v, want the labels of the event", labels)
					}

					return tc.service, tc.loaded
				},
			}
			dc := deployConfig.New("stack-a", "main")
			req := DeployRequest{
				Logger:        log,
				Metadata:      notification.Metadata{Repository: t.Name()},
				DeployConfigs: []*deployConfig.Config{dc},
			}
			j := newJob(manager, req, getDeployConfigGroupByEvent(req.DeployConfigs))

			event := events.Message{
				Type:   events.ContainerEventType,
				Action: "kill",
				Actor: events.Actor{
					ID: "web-1",
					Attributes: map[string]string{
						"name":           "stack-a-web-1",
						api.ProjectLabel: "stack-a",
						api.ServiceLabel: "web",
					},
				},
			}
			if tc.hooksLabel != nil {
				event.Actor.Attributes[docker.DocoCDLabels.Deployment.LifecycleHooks] = *tc.hooksLabel
			}

			result := j.restartContainer(t.Context(), log, event, dc, cli, tc.swarm)
			if result.fallbackToDeploy {
				t.Fatal("restartContainer() fell back to a redeploy")
			}

			if got := apiClient.recorded(); !slices.Equal(got, tc.wantCalls) {
				t.Fatalf("calls = %q, want %q; logs: %s", got, tc.wantCalls, output.String())
			}

			if loads != tc.wantLoads {
				t.Fatalf("hook loads = %d, want %d", loads, tc.wantLoads)
			}

			if !slices.Equal(notifier.sent, tc.wantNotifications) {
				t.Fatalf("notifications = %+v, want %+v", notifier.sent, tc.wantNotifications)
			}

			if tc.wantLog != "" && !strings.Contains(output.String(), tc.wantLog) {
				t.Fatalf("logs do not contain %q: %s", tc.wantLog, output.String())
			}

			if suppressed, _ := j.shouldSuppressRestartFollowupEvent("die", event); !suppressed {
				t.Fatal("follow-up die event of the restart is not suppressed")
			}
		})
	}
}

// startupScanQueries lists fixed containers. All of them are unhealthy.
type startupScanQueries struct {
	RuntimeQueries

	containers []container.Summary
}

func (q startupScanQueries) ListManagedRepositoryContainers(context.Context, client.APIClient, string, bool) ([]container.Summary, error) {
	return q.containers, nil
}

func (q startupScanQueries) InspectContainerState(context.Context, client.APIClient, string) (*container.State, error) {
	return &container.State{Running: true, Health: &container.Health{Status: container.Unhealthy}}, nil
}

func TestRestartUnhealthyContainersOnStartupRunsComposeLifecycleHooks(t *testing.T) {
	t.Parallel()

	serviceLabels := map[string]string{
		docker.DocoCDLabels.Deployment.Name: "stack-a",
		api.ProjectLabel:                    "stack-a",
		api.ServiceLabel:                    "web",
	}

	runnerLabels := maps.Clone(serviceLabels)
	runnerLabels[api.HookLabel] = "pre_start"

	log := slog.New(slog.DiscardHandler)
	apiClient := &restartHookClient{}

	var loadedLabels []map[string]string

	manager := &Manager{
		notifier:    &recordingNotifier{},
		deployments: deploymentTracker{stacks: make(map[string]int)},
		runtimeQueries: startupScanQueries{containers: []container.Summary{
			{ID: "web-1", Names: []string{"/stack-a-web-1"}, Labels: serviceLabels},
			{ID: "runner-1", Names: []string{"/stack-a-web-pre_start-1"}, Labels: runnerLabels},
		}},
		loadHookService: func(_ context.Context, _ *slog.Logger, _ command.Cli, labels map[string]string) (types.ServiceConfig, bool) {
			loadedLabels = append(loadedLabels, labels)

			return types.ServiceConfig{
				Name:      "web",
				PostStart: []types.ServiceHook{{Command: types.ShellCommand{"post_start"}}},
				PreStop:   []types.ServiceHook{{Command: types.ShellCommand{"pre_stop"}}},
			}, true
		},
	}

	dc := deployConfig.New("stack-a", "main")
	req := DeployRequest{Logger: log, DeployConfigs: []*deployConfig.Config{dc}}
	j := newJob(manager, req, getDeployConfigGroupByEvent(req.DeployConfigs))

	j.restartUnhealthyContainersOnStartup(t.Context(), log, swarmEventNameCLI{client: apiClient}, false, []*deployConfig.Config{dc})

	// The hook runner is not a service replica. Only the service container restarts.
	wantCalls := []string{"exec web-1 pre_stop", "restart web-1", "exec web-1 post_start"}
	if got := apiClient.recorded(); !slices.Equal(got, wantCalls) {
		t.Fatalf("calls = %q, want %q", got, wantCalls)
	}

	if len(loadedLabels) != 1 || loadedLabels[0][api.ServiceLabel] != "web" || loadedLabels[0]["name"] != "stack-a-web-1" {
		t.Fatalf("hook loader got labels %v, want the container labels and its name", loadedLabels)
	}
}
