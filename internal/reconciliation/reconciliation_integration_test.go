package reconciliation

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	dockerSwarm "github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/stages"
	internaltest "github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

const dockerIntegrationEnvVar = "DOCO_CD_RUN_DOCKER_INTEGRATION_TESTS"

func TestReconciliationDockerEventActions(t *testing.T) {
	requireDockerIntegrationTestGate(t)

	ctx := t.Context()

	tests := []struct {
		name        string
		wantAction  string
		composeYAML string
		trigger     func(context.Context, *testing.T, *internaltest.ComposeStack)
	}{
		{
			name:        "die",
			wantAction:  "die",
			composeYAML: runningServiceComposeYAML(),
			trigger: func(ctx context.Context, t *testing.T, stack *internaltest.ComposeStack) {
				t.Helper()
				containerID := stack.ServiceContainerID(ctx, t, "app")

				_, err := stack.Client.ContainerKill(ctx, containerID, client.ContainerKillOptions{Signal: "SIGKILL"})
				if err != nil {
					t.Fatalf("failed to kill container %s: %v", containerID, err)
				}
			},
		},
		{
			name:        "destroy",
			wantAction:  "destroy",
			composeYAML: runningServiceComposeYAML(),
			trigger: func(ctx context.Context, t *testing.T, stack *internaltest.ComposeStack) {
				t.Helper()
				containerID := stack.ServiceContainerID(ctx, t, "app")

				_, err := stack.Client.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})
				if err != nil {
					t.Fatalf("failed to remove container %s: %v", containerID, err)
				}
			},
		},
		{
			name:        "stop",
			wantAction:  "stop",
			composeYAML: runningServiceComposeYAML(),
			trigger: func(ctx context.Context, t *testing.T, stack *internaltest.ComposeStack) {
				t.Helper()
				containerID := stack.ServiceContainerID(ctx, t, "app")
				timeout := 1

				_, err := stack.Client.ContainerStop(ctx, containerID, client.ContainerStopOptions{Timeout: &timeout})
				if err != nil {
					t.Fatalf("failed to stop container %s: %v", containerID, err)
				}
			},
		},
		{
			name:        "kill",
			wantAction:  "kill",
			composeYAML: runningServiceComposeYAML(),
			trigger: func(ctx context.Context, t *testing.T, stack *internaltest.ComposeStack) {
				t.Helper()
				containerID := stack.ServiceContainerID(ctx, t, "app")

				_, err := stack.Client.ContainerKill(ctx, containerID, client.ContainerKillOptions{Signal: "SIGKILL"})
				if err != nil {
					t.Fatalf("failed to kill container %s: %v", containerID, err)
				}
			},
		},
		{
			name:        "oom",
			wantAction:  "oom",
			composeYAML: oomServiceComposeYAML(),
			trigger: func(ctx context.Context, t *testing.T, stack *internaltest.ComposeStack) {
				t.Helper()

				go func() {
					_, _ = stack.Exec(ctx, t, "app", []string{"python", "-c", "chunks=[]\nwhile True:\n    chunks.append('x' * 1024 * 1024)"})
				}()
			},
		},
		{
			name:        "health_status_unhealthy",
			wantAction:  "unhealthy",
			composeYAML: unhealthyOnDemandComposeYAML(),
			trigger: func(ctx context.Context, t *testing.T, stack *internaltest.ComposeStack) {
				t.Helper()

				if exitCode, _ := stack.Exec(ctx, t, "app", []string{"sh", "-c", "rm -f /tmp/healthy"}); exitCode != 0 {
					t.Fatalf("expected health-trigger command to succeed, got exit code %d", exitCode)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stackName := internaltest.ConvertTestName(t.Name())
			stack := internaltest.ComposeUp(ctx, t,
				internaltest.WithName(stackName),
				internaltest.WithYAML(tt.composeYAML),
				internaltest.WithWaitTimeout(90*time.Second),
			)

			waitForExpectedDockerEvent(ctx, t, stack.Client, stack.Name, tt.wantAction, func() {
				tt.trigger(ctx, t, stack)
			})
		})
	}
}

func TestReconciliationStopEventRestartSuppressionIntegration(t *testing.T) {
	requireDockerIntegrationTestGate(t)

	ctx := t.Context()
	stackName := internaltest.ConvertTestName(t.Name())
	repositoryName := "kimdre/doco-cd_tests"

	stack := internaltest.ComposeUp(ctx, t,
		internaltest.WithName(stackName),
		internaltest.WithYAML(restartMarkerComposeYAML()),
		internaltest.WithWaitTimeout(90*time.Second),
		internaltest.WithCustomLabel(map[string]string{
			docker.DocoCDLabels.Metadata.Manager:     config.AppName,
			docker.DocoCDLabels.Repository.Name:      repositoryName,
			docker.DocoCDLabels.Deployment.Name:      stackName,
			docker.DocoCDLabels.Deployment.TargetRef: "main",
		}),
	)

	logSince := time.Now().Add(-2 * time.Second)
	waitForBootMarkerCount(ctx, t, stack, logSince, 1, 20*time.Second)

	dc := config.DefaultDeployConfig(stackName, "main")
	dc.Reconciliation.Enabled = true
	dc.Reconciliation.Events = []string{"stop"}
	dc.Reconciliation.RestartTimeout = 1

	jobLog := logger.New(slog.LevelError).Logger
	reconcileJob := newJob(jobInfo{
		jobLog:        jobLog,
		dockerCli:     stack.DockerCli,
		metadata:      notification.Metadata{Repository: repositoryName, Stack: stackName, JobID: "test-job"},
		repoData:      stages.RepositoryData{CloneURL: config.HttpUrl("https://github.com/kimdre/doco-cd_tests.git"), Name: repositoryName},
		payload:       &webhook.ParsedPayload{FullName: repositoryName},
		deployConfigs: []*config.DeployConfig{dc},
	}, getDeployConfigGroupByEvent([]*config.DeployConfig{dc}))

	runCtx, cancel := context.WithCancel(ctx)

	t.Cleanup(func() {
		cancel()
		reconcileJob.close()
	})

	go reconcileJob.run(runCtx)

	// Give the reconciliation listener a brief moment to subscribe before triggering the restart.
	time.Sleep(1 * time.Second)

	containerID := stack.ServiceContainerID(ctx, t, "app")
	restartTimeout := 1

	if _, err := stack.Client.ContainerRestart(ctx, containerID, client.ContainerRestartOptions{Timeout: &restartTimeout}); err != nil {
		t.Fatalf("failed to restart container %s: %v", containerID, err)
	}

	// Initial start + user restart + one reconciliation restart.
	waitForBootMarkerCount(ctx, t, stack, logSince, 3, 35*time.Second)

	// Regression assertion: no additional restart loop should happen after follow-up stop/die/kill events.
	assertBootMarkerCountStable(ctx, t, stack, logSince, 3, 6*time.Second)
}

func requireDockerIntegrationTestGate(t *testing.T) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping Docker reconciliation integration tests in short mode")
	}

	if os.Getenv(dockerIntegrationEnvVar) != "1" {
		t.Skipf("set %s=1 to run Docker reconciliation integration tests", dockerIntegrationEnvVar)
	}

	dockerCli, err := internaltest.NewDockerCli()
	if err != nil {
		t.Skipf("skipping Docker reconciliation integration tests: %v", err)
	}

	defer func() {
		_ = dockerCli.Client().Close()
	}()

	if err := dockerSwarm.RefreshModeEnabled(t.Context(), dockerCli.Client()); err != nil {
		t.Fatalf("failed to inspect Docker swarm mode: %v", err)
	}

	if dockerSwarm.GetModeEnabled() {
		t.Skip("reconciliation Docker event integration tests require non-Swarm mode")
	}
}

func waitForExpectedDockerEvent(ctx context.Context, t *testing.T, cli client.APIClient, stackName, wantAction string, trigger func()) {
	t.Helper()

	filterArgs := make(client.Filters)
	filterArgs.Add("type", "container")
	filterArgs.Add("label", fmt.Sprintf("%s=%s", api.ProjectLabel, stackName))

	listenerCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	eventsResult := cli.Events(listenerCtx, client.EventsListOptions{Filters: filterArgs})

	trigger()

	seen := map[string]struct{}{}

	for {
		select {
		case msg, ok := <-eventsResult.Messages:
			if !ok {
				t.Fatalf("docker events channel closed before observing %q, seen=%v", wantAction, mapKeys(seen))
			}

			action := normalizeReconciliationEventAction(string(msg.Action))

			seen[action] = struct{}{}
			if action == wantAction {
				return
			}
		case err, ok := <-eventsResult.Err:
			if !ok {
				t.Fatalf("docker events error channel closed before observing %q, seen=%v", wantAction, mapKeys(seen))
			}

			if err != nil {
				t.Fatalf("docker events listener failed while waiting for %q: %v (seen=%v)", wantAction, err, mapKeys(seen))
			}
		case <-listenerCtx.Done():
			t.Fatalf("timed out waiting for docker event %q, seen=%v", wantAction, mapKeys(seen))
		}
	}
}

func mapKeys(m map[string]struct{}) []string {
	ret := make([]string, 0, len(m))
	for key := range m {
		ret = append(ret, key)
	}

	return ret
}

func waitForBootMarkerCount(ctx context.Context, t *testing.T, stack *internaltest.ComposeStack, since time.Time, want int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		if time.Now().After(deadline) {
			got := bootMarkerCount(stack.ContainerLogs(ctx, t, "app", since))
			t.Fatalf("timed out waiting for %d boot markers, got %d", want, got)
		}

		got := bootMarkerCount(stack.ContainerLogs(ctx, t, "app", since))
		if got >= want {
			return
		}

		time.Sleep(250 * time.Millisecond)
	}
}

func assertBootMarkerCountStable(ctx context.Context, t *testing.T, stack *internaltest.ComposeStack, since time.Time, expected int, duration time.Duration) {
	t.Helper()

	deadline := time.Now().Add(duration)

	for {
		got := bootMarkerCount(stack.ContainerLogs(ctx, t, "app", since))
		if got != expected {
			t.Fatalf("expected boot marker count to remain %d, got %d", expected, got)
		}

		if time.Now().After(deadline) {
			return
		}

		time.Sleep(250 * time.Millisecond)
	}
}

func bootMarkerCount(logs string) int {
	return strings.Count(logs, "BOOT_MARKER")
}

func runningServiceComposeYAML() string {
	return `services:
  app:
    image: alpine:3.20
    restart: "no"
    command: ["sh", "-c", "trap : TERM INT; while true; do sleep 1; done"]
`
}

func oomServiceComposeYAML() string {
	return `services:
  app:
    image: python:3.12-alpine
    restart: "no"
    mem_limit: 64m
    command: ["python", "-c", "import time; time.sleep(3600)"]
`
}

func unhealthyOnDemandComposeYAML() string {
	return `services:
  app:
    image: alpine:3.20
    restart: "no"
    command: ["sh", "-c", "touch /tmp/healthy; trap : TERM INT; while true; do sleep 1; done"]
    healthcheck:
      test: ["CMD-SHELL", "test -f /tmp/healthy"]
      interval: 1s
      timeout: 1s
      retries: 1
      start_period: 1s
`
}

func restartMarkerComposeYAML() string {
	return `services:
  app:
	image: alpine:3.20
	restart: "no"
	command: ["sh", "-c", "echo BOOT_MARKER; trap : TERM INT; while true; do sleep 1; done"]
`
}
