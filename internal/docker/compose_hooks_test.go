package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
	composeCli "github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestComposeHookOutputTail(t *testing.T) {
	t.Parallel()

	tail := &composeHookOutputTail{max: 5}

	for _, chunk := range []string{"ab", "cd", "efg"} {
		if n, err := tail.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}

	if got := tail.String(); got != "cdefg" {
		t.Fatalf("tail after small writes = %q, want %q", got, "cdefg")
	}

	if _, err := tail.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}

	if got := tail.String(); got != "56789" {
		t.Fatalf("tail after a large write = %q, want %q", got, "56789")
	}
}

func TestRunComposeHooks_WithoutHooksDoesNotCallDocker(t *testing.T) {
	t.Parallel()

	// A nil client panics if RunComposeHooks calls the Docker API.
	if err := RunComposeHooks(t.Context(), nil, "container", types.ServiceConfig{Name: "web"}, ComposeHookPreStop); err != nil {
		t.Fatalf("RunComposeHooks() without hooks = %v", err)
	}
}

func shellHook(script string) types.ServiceHook {
	return types.ServiceHook{Command: types.ShellCommand{"sh", "-c", script}}
}

func TestRunComposeHooks(t *testing.T) {
	t.Parallel()

	apiClient := setupContainerWaitTest(t)
	ctx := t.Context()

	containerID := createTestContainer(ctx, t, apiClient, []string{"sleep", "300"})

	t.Run("stopped container", func(t *testing.T) {
		svc := types.ServiceConfig{Name: "web", PreStop: []types.ServiceHook{shellHook("true")}}

		err := RunComposeHooks(ctx, apiClient, containerID, svc, ComposeHookPreStop)
		if !errors.Is(err, ErrComposeHookFailed) || !IsComposeHookContainerUnavailable(err) {
			t.Fatalf("RunComposeHooks() on a stopped container = %v, want a hook error for an unavailable container", err)
		}
	})

	if _, err := apiClient.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	t.Run("runs hooks in order with their settings", func(t *testing.T) {
		hook := shellHook(`echo "$HOOK_VALUE:$(id -u):$(pwd)" > /tmp/first`)
		hook.User = "nobody"
		hook.WorkingDir = "/tmp"
		hook.Environment = types.NewMappingWithEquals([]string{"HOOK_VALUE=from-hook"})

		svc := types.ServiceConfig{
			Name: "web",
			PostStart: []types.ServiceHook{
				hook,
				shellHook(`test "$(cat /tmp/first)" = "from-hook:65534:/tmp"`),
			},
		}

		if err := RunComposeHooks(ctx, apiClient, containerID, svc, ComposeHookPostStart); err != nil {
			t.Fatalf("RunComposeHooks() = %v", err)
		}
	})

	t.Run("failed hook reports stderr and stops the sequence", func(t *testing.T) {
		svc := types.ServiceConfig{
			Name: "web",
			PreStop: []types.ServiceHook{
				shellHook("echo ignored-output; echo hook-error >&2; exit 3"),
				shellHook("touch /tmp/after-failure"),
			},
		}

		err := RunComposeHooks(ctx, apiClient, containerID, svc, ComposeHookPreStop)
		if !errors.Is(err, ErrComposeHookFailed) {
			t.Fatalf("RunComposeHooks() = %v, want ErrComposeHookFailed", err)
		}

		for _, want := range []string{`service "web" pre_stop[0]`, "status 3", "hook-error"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not contain %q", err, want)
			}
		}

		if strings.Contains(err.Error(), "ignored-output") || IsComposeHookContainerUnavailable(err) {
			t.Fatalf("unexpected error details: %v", err)
		}

		check := types.ServiceConfig{Name: "web", PreStop: []types.ServiceHook{shellHook("test ! -e /tmp/after-failure")}}
		if err := RunComposeHooks(ctx, apiClient, containerID, check, ComposeHookPreStop); err != nil {
			t.Fatalf("hook after the failed hook ran: %v", err)
		}
	})

	t.Run("failed hook without stderr reports stdout", func(t *testing.T) {
		svc := types.ServiceConfig{Name: "web", PreStop: []types.ServiceHook{shellHook("echo only-output; exit 2")}}

		err := RunComposeHooks(ctx, apiClient, containerID, svc, ComposeHookPreStop)
		if err == nil || !strings.Contains(err.Error(), "status 2: only-output") {
			t.Fatalf("RunComposeHooks() = %v, want the stdout tail", err)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		hookCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()

		svc := types.ServiceConfig{Name: "web", PreStop: []types.ServiceHook{shellHook("sleep 30")}}

		started := time.Now()

		err := RunComposeHooks(hookCtx, apiClient, containerID, svc, ComposeHookPreStop)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("RunComposeHooks() = %v, want context.DeadlineExceeded", err)
		}

		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Fatalf("RunComposeHooks() returned after %s, want it to stop at the deadline", elapsed)
		}
	})
}

const composeHooksTestYAML = `services:
  db:
    image: alpine:latest
    command: ["sleep", "300"]
    stop_grace_period: 1s
    volumes:
      - marks:/marks
    pre_start:
      - command: ["sh", "-c", "echo pre_start >> /marks/log"]
    post_start:
      - command: ["sh", "-c", "echo post_start >> /marks/log"]
    pre_stop:
      - command: ["sh", "-c", "echo pre_stop >> /marks/log"]
  app:
    image: alpine:latest
    command: ["sleep", "300"]
    stop_grace_period: 1s
    labels:
      # A deployed service without hooks. doco-cd must not load its project.
      cd.doco.deployment.lifecycle_hooks: ""
volumes:
  marks:
`

// composeHooksTestLoader loads the test project from its Compose file instead of doco-cd deployment metadata.
func composeHooksTestLoader(t *testing.T, composeFile, projectName string, loads *int) *ComposeHookLoader {
	t.Helper()

	return &ComposeHookLoader{
		loadProject: func(ctx context.Context, _ command.Cli, labels map[string]string) (*types.Project, error) {
			*loads++

			options, err := composeCli.NewProjectOptions(
				[]string{composeFile},
				composeCli.WithWorkingDirectory(filepath.Dir(composeFile)),
				composeCli.WithName(projectName),
			)
			if err != nil {
				return nil, err
			}

			project, err := options.LoadProject(ctx)
			if err != nil {
				return nil, err
			}

			return project.WithSelectedServices([]string{labels[api.ServiceLabel]}, types.IgnoreDependencies)
		},
	}
}

func TestStopAndStartProjectServices_RunComposeHooks(t *testing.T) {
	ctx := t.Context()

	composeFile := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(composeFile, []byte(composeHooksTestYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	stack := test.ComposeUp(ctx, t, test.WithFile(composeFile))
	dockerCli := stack.DockerCli

	loads := 0
	hooks := composeHooksTestLoader(t, composeFile, stack.Name, &loads)

	serviceContainer := func(serviceName string) hookTestContainer {
		t.Helper()

		containers, err := GetProjectContainers(ctx, dockerCli, stack.Name)
		if err != nil {
			t.Fatal(err)
		}

		for _, c := range containers {
			if c.Labels[api.ServiceLabel] == serviceName {
				return hookTestContainer{id: c.ID, state: string(c.State)}
			}
		}

		t.Fatalf("service %q has no container", serviceName)

		return hookTestContainer{}
	}

	assertLog := func(want ...string) {
		t.Helper()

		check := types.ServiceConfig{
			Name:    "db",
			PreStop: []types.ServiceHook{shellHook(`test "$(cat /marks/log | tr '\n' ' ')" = "$WANT "`)},
		}
		check.PreStop[0].Environment = types.NewMappingWithEquals([]string{"WANT=" + strings.Join(want, " ")})

		if err := RunComposeHooks(ctx, dockerCli.Client(), serviceContainer("db").id, check, ComposeHookPreStop); err != nil {
			t.Fatalf("hook log does not match %v: %v", want, err)
		}
	}

	assertLog("pre_start", "post_start")

	timeout := time.Second
	if err := StopProjectServices(ctx, dockerCli, stack.Name, []string{"db", "app"}, &timeout, hooks); err != nil {
		t.Fatalf("StopProjectServices() = %v", err)
	}

	assertComposeHookServiceStates(t, dockerCli, stack.Name, map[string]string{"db": "exited", "app": "exited"})

	if err := StartProjectServices(ctx, dockerCli, stack.Name, []string{"db", "app"}, hooks); err != nil {
		t.Fatalf("StartProjectServices() = %v", err)
	}

	assertComposeHookServiceStates(t, dockerCli, stack.Name, map[string]string{"db": "running", "app": "running"})

	assertLog("pre_start", "post_start", "pre_stop", "pre_start", "post_start")

	// Compose removes the pre_start runners after a successful start.
	runners, err := GetLabeledContainers(ctx, dockerCli.Client(), api.ProjectLabel, stack.Name, true)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range runners {
		if c.Labels[api.HookLabel] != "" {
			t.Fatalf("hook runner %s (%s) was not consumed", c.ID[:12], c.State)
		}
	}

	// db has no lifecycle hooks label, like a container from an older doco-cd version, so its project loads
	// once per operation. The label of app shows that it has no hooks, so its project never loads.
	if loads != 2 {
		t.Fatalf("project loads = %d, want one per operation for db only", loads)
	}
}

func TestComposeHookLabelValue(t *testing.T) {
	hook := []types.ServiceHook{shellHook("true")}

	tests := []struct {
		name    string
		service types.ServiceConfig
		want    string
	}{
		{name: "no hooks", service: types.ServiceConfig{}, want: ""},
		{name: "pre_stop", service: types.ServiceConfig{PreStop: hook}, want: "pre_stop"},
		{
			name: "all phases",
			service: types.ServiceConfig{
				PreStart:  []types.PreStartHook{{}},
				PostStart: hook,
				PreStop:   hook,
			},
			want: "pre_start,post_start,pre_stop",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := composeHookLabelValue(tc.service); got != tc.want {
				t.Fatalf("composeHookLabelValue() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMayHaveComposeHooks(t *testing.T) {
	label := DocoCDLabels.Deployment.LifecycleHooks

	tests := []struct {
		name   string
		labels map[string]string
		phases []ComposeHookPhase
		want   bool
	}{
		{name: "no label", labels: map[string]string{}, phases: []ComposeHookPhase{ComposeHookPreStop}, want: true},
		{name: "empty label", labels: map[string]string{label: ""}, phases: []ComposeHookPhase{ComposeHookPreStop}, want: false},
		{name: "other phase", labels: map[string]string{label: "pre_start"}, phases: []ComposeHookPhase{ComposeHookPreStop, ComposeHookPostStart}, want: false},
		{name: "matching phase", labels: map[string]string{label: "pre_start,post_start"}, phases: []ComposeHookPhase{ComposeHookPreStop, ComposeHookPostStart}, want: true},
		{name: "spaces", labels: map[string]string{label: "pre_start, pre_stop"}, phases: []ComposeHookPhase{ComposeHookPreStop}, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MayHaveComposeHooks(tc.labels, tc.phases...); got != tc.want {
				t.Fatalf("MayHaveComposeHooks() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAddComposeServiceLabels_LifecycleHooks(t *testing.T) {
	project := &types.Project{
		Name: "hooks",
		Services: types.Services{
			"db":  {Name: "db", PreStop: []types.ServiceHook{shellHook("true")}},
			"app": {Name: "app"},
		},
	}

	addComposeServiceLabels(project, &deploy.Config{}, &webhook.ParsedPayload{}, "", t.TempDir(), "test",
		time.Now().UTC().Format(time.RFC3339), ComposeVersion, "", "")

	for name, want := range map[string]string{"db": "pre_stop", "app": ""} {
		got, ok := project.Services[name].CustomLabels[DocoCDLabels.Deployment.LifecycleHooks]
		if !ok || got != want {
			t.Fatalf("service %q lifecycle hooks label = %q (set: %v), want %q", name, got, ok, want)
		}
	}
}

func TestStopProjectServices_FailedPreStopKeepsContainerRunning(t *testing.T) {
	ctx := t.Context()

	composeFile := filepath.Join(t.TempDir(), "compose.yaml")

	yaml := strings.ReplaceAll(composeHooksTestYAML, "echo pre_stop >> /marks/log", "echo pre-stop-failed >&2; exit 4")
	if err := os.WriteFile(composeFile, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	stack := test.ComposeUp(ctx, t, test.WithFile(composeFile))
	dockerCli := stack.DockerCli

	loads := 0
	hooks := composeHooksTestLoader(t, composeFile, stack.Name, &loads)

	timeout := time.Second

	err := StopProjectServices(ctx, dockerCli, stack.Name, []string{"db", "app"}, &timeout, hooks)
	if err == nil || !strings.Contains(err.Error(), "pre-stop-failed") {
		t.Fatalf("StopProjectServices() = %v, want the pre_stop failure", err)
	}

	// Like Compose, the failed hook keeps its container running. Other services still stop.
	assertComposeHookServiceStates(t, dockerCli, stack.Name, map[string]string{"db": "running", "app": "exited"})
}

func TestRestartComposeScheduledContainerRunsComposeHooks(t *testing.T) {
	ctx := t.Context()

	composeFile := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(composeFile, []byte(composeHooksTestYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	stack := test.ComposeUp(ctx, t, test.WithFile(composeFile))

	containers, err := GetProjectContainers(ctx, stack.DockerCli, stack.Name)
	if err != nil {
		t.Fatal(err)
	}

	containerID := ""

	for _, c := range containers {
		if c.Labels[api.ServiceLabel] == "db" {
			containerID = c.ID
			break
		}
	}

	if containerID == "" {
		t.Fatal("db container is missing")
	}

	apiClient := stack.DockerCli.Client()
	svc := types.ServiceConfig{
		Name:      "db",
		PreStart:  []types.PreStartHook{{}},
		PreStop:   []types.ServiceHook{shellHook("echo restart_pre_stop >> /marks/log")},
		PostStart: []types.ServiceHook{shellHook("echo restart_post_start >> /marks/log")},
	}

	if err := restartComposeScheduledContainer(ctx, apiClient, containerID, svc, false); err != nil {
		t.Fatalf("restart with hooks: %v", err)
	}

	check := types.ServiceConfig{Name: "db", PreStop: []types.ServiceHook{
		shellHook(`test "$(cat /marks/log | tr '\n' ' ')" = "pre_start post_start restart_pre_stop restart_post_start "`),
	}}
	if err := RunComposeHooks(ctx, apiClient, containerID, check, ComposeHookPreStop); err != nil {
		t.Fatalf("restart did not run only pre_stop and post_start in order: %v", err)
	}

	before, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	svc.PreStop = []types.ServiceHook{shellHook("echo failed-pre-stop >&2; exit 3")}

	err = restartComposeScheduledContainer(ctx, apiClient, containerID, svc, false)
	if !errors.Is(err, ErrComposeHookFailed) {
		t.Fatalf("restart error = %v, want a pre_stop failure", err)
	}

	after, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if !after.Container.State.Running || after.Container.State.StartedAt != before.Container.State.StartedAt {
		t.Fatal("failed pre_stop did not leave the running job unchanged")
	}
}

type hookTestContainer struct {
	id    string
	state string
}

// Docker's container list can lag behind stop and start operations.
func assertComposeHookServiceStates(t *testing.T, dockerCli command.Cli, projectName string, want map[string]string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	stateMismatch := errors.New("service states have not converged")

	var observed map[string][]hookTestContainer

	err := retry.New(
		retry.Context(ctx),
		retry.Attempts(20),
		retry.Delay(500*time.Millisecond),
		retry.DelayType(retry.FixedDelay),
		retry.LastErrorOnly(true),
		retry.RetryIf(func(err error) bool { return errors.Is(err, stateMismatch) }),
	).Do(func() error {
		containers, err := GetProjectContainers(ctx, dockerCli, projectName)
		if err != nil {
			return err
		}

		observed = make(map[string][]hookTestContainer)

		for _, c := range containers {
			service := c.Labels[api.ServiceLabel]
			observed[service] = append(observed[service], hookTestContainer{
				id:    c.ID[:min(12, len(c.ID))],
				state: string(c.State),
			})
		}

		for service, state := range want {
			if len(observed[service]) == 0 {
				return stateMismatch
			}

			for _, c := range observed[service] {
				if c.state != state {
					return stateMismatch
				}
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("project %q service states: want %v, observed %+v: %v", projectName, want, observed, err)
	}
}
