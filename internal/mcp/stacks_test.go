package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
	"time"

	dockerswarmtypes "github.com/moby/moby/api/types/swarm"
	dockerclient "github.com/moby/moby/client"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	dockerswarm "github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
)

func TestMCPStackToolValidation(t *testing.T) {
	h := &Handler{log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	for _, testCase := range []struct {
		name      string
		tool      string
		arguments map[string]any
		contains  string
	}{
		{name: "control missing stack", tool: "control_stack", arguments: map[string]any{"action": "restart"}, contains: "stack_name"},
		{name: "control blank stack", tool: "control_stack", arguments: map[string]any{"stack_name": "  ", "action": "restart"}, contains: "missing stack name"},
		{name: "control missing action", tool: "control_stack", arguments: map[string]any{"stack_name": "stack"}, contains: "action"},
		{name: "control invalid action", tool: "control_stack", arguments: map[string]any{"stack_name": "stack", "action": "invalid"}, contains: "enum"},
		{name: "scale missing replicas", tool: "control_stack", arguments: map[string]any{"stack_name": "stack", "action": "scale"}, contains: "replicas"},
		{name: "scale negative replicas", tool: "control_stack", arguments: map[string]any{"stack_name": "stack", "action": "scale", "replicas": -1}, contains: "minimum"},
		{name: "restart ignores omitted replicas", tool: "control_stack", arguments: map[string]any{"stack_name": "stack", "action": "restart"}, contains: "docker cli is required"},
		{name: "remove missing stack", tool: "remove_stack", arguments: map[string]any{}, contains: "stack_name"},
		{name: "remove blank stack", tool: "remove_stack", arguments: map[string]any{"stack_name": "  "}, contains: "missing stack name"},
		{name: "remove nil docker", tool: "remove_stack", arguments: map[string]any{"stack_name": "stack"}, contains: "docker cli is required"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			assertMCPToolError(t, session, testCase.tool, testCase.arguments, testCase.contains)
		})
	}
}

func TestRunStackActionReturnsSkippedServices(t *testing.T) {
	services := []dockerswarmtypes.Service{
		{Spec: dockerswarmtypes.ServiceSpec{Annotations: dockerswarmtypes.Annotations{Name: "stack_web"}, Mode: dockerswarmtypes.ServiceMode{Replicated: &dockerswarmtypes.ReplicatedService{}}}},
		{Spec: dockerswarmtypes.ServiceSpec{Annotations: dockerswarmtypes.Annotations{Name: "stack_job"}, Mode: dockerswarmtypes.ServiceMode{ReplicatedJob: &dockerswarmtypes.ReplicatedJob{}}}},
		{Spec: dockerswarmtypes.ServiceSpec{Annotations: dockerswarmtypes.Annotations{Name: "stack_global"}, Mode: dockerswarmtypes.ServiceMode{Global: &dockerswarmtypes.GlobalService{}}}},
	}

	for _, testCase := range []struct {
		name       string
		action     string
		service    string
		wantStatus string
		wantReason string
	}{
		{name: "restart job", action: "restart", service: "job", wantStatus: "skipped", wantReason: docker.ErrJobServiceRestartNotSupported.Error()},
		{name: "run replicated service", action: "run", service: "web", wantStatus: "skipped", wantReason: docker.ErrNotAJobService.Error()},
		{name: "scale global service", action: "scale", service: "global", wantStatus: "skipped", wantReason: dockerswarm.ErrNotReplicatedService.Error()},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newStackActionTestHandler(t, services)

			results, err := controlplane.RunStackAction(t.Context(), h.dockerCli, "stack", testCase.action, testCase.service, 0, true, slog.Default())
			if !errors.Is(err, controlplane.ErrNoApplicableStackServices) {
				t.Fatalf("error = %v, want no applicable services", err)
			}

			if len(results) != 1 || results[0].Service != "stack_"+testCase.service || results[0].Status != testCase.wantStatus || results[0].Reason != testCase.wantReason {
				t.Fatalf("unexpected results: %#v", results)
			}
		})
	}
}

func TestRunStackActionRejectsMissingService(t *testing.T) {
	services := []dockerswarmtypes.Service{{Spec: dockerswarmtypes.ServiceSpec{Annotations: dockerswarmtypes.Annotations{Name: "stack_web"}}}}

	for _, action := range []string{"scale", "restart", "run"} {
		t.Run(action, func(t *testing.T) {
			h := newStackActionTestHandler(t, services)
			results, err := controlplane.RunStackAction(t.Context(), h.dockerCli, "stack", action, "missing", 1, true, slog.Default())

			var notFound *controlplane.StackServiceNotFoundError
			if !errors.As(err, &notFound) || notFound.Service != "stack_missing" || len(results) != 0 {
				t.Fatalf("results = %#v, error = %#v", results, err)
			}
		})
	}
}

func TestRunStackActionReturnsPartialResultsOnHardError(t *testing.T) {
	services := []dockerswarmtypes.Service{
		{ID: "first-id", Spec: dockerswarmtypes.ServiceSpec{Annotations: dockerswarmtypes.Annotations{Name: "stack_first"}, Mode: dockerswarmtypes.ServiceMode{Replicated: &dockerswarmtypes.ReplicatedService{}}}},
		{ID: "second-id", Spec: dockerswarmtypes.ServiceSpec{Annotations: dockerswarmtypes.Annotations{Name: "stack_second"}, Mode: dockerswarmtypes.ServiceMode{Replicated: &dockerswarmtypes.ReplicatedService{}}}},
		{ID: "third-id", Spec: dockerswarmtypes.ServiceSpec{Annotations: dockerswarmtypes.Annotations{Name: "stack_third"}, Mode: dockerswarmtypes.ServiceMode{Replicated: &dockerswarmtypes.ReplicatedService{}}}},
	}

	var inspectedThird bool

	h := newStackActionTestHandlerWithFailure(t, services, "stack_second", &inspectedThird)

	results, err := controlplane.RunStackAction(t.Context(), h.dockerCli, "stack", "restart", "", 0, true, slog.Default())

	var actionErr *controlplane.StackServiceActionError
	if !errors.As(err, &actionErr) || actionErr.Service != "stack_second" || !strings.Contains(err.Error(), "forced inspect failure") {
		t.Fatalf("error = %#v", err)
	}

	if len(results) != 1 || results[0] != (controlplane.StackActionResult{Service: "stack_first", Status: "ok"}) {
		t.Fatalf("results = %#v", results)
	}

	if inspectedThird {
		t.Fatal("action continued after hard failure")
	}
}

func TestControlStackReturnsStructuredOperationalErrors(t *testing.T) {
	job := dockerswarmtypes.Service{ID: "job-id", Spec: dockerswarmtypes.ServiceSpec{Annotations: dockerswarmtypes.Annotations{Name: "stack_job"}, Mode: dockerswarmtypes.ServiceMode{ReplicatedJob: &dockerswarmtypes.ReplicatedJob{}}}}
	h := newStackActionTestHandler(t, []dockerswarmtypes.Service{job})

	result, output, err := h.controlStack(t.Context(), nil, controlStackInput{StackName: "stack", Action: "restart"})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("result = %#v, output = %#v, error = %v", result, output, err)
	}

	if output.StackName != "stack" || output.Action != "restart" || len(output.Results) != 1 || output.Results[0].Status != "skipped" {
		t.Fatalf("structured output = %#v", output)
	}

	if len(result.Content) != 1 || !strings.Contains(result.Content[0].(*sdkmcp.TextContent).Text, "no applicable") {
		t.Fatalf("content = %#v", result.Content)
	}
}

func TestControlStackReturnsMissingServiceErrorContent(t *testing.T) {
	service := dockerswarmtypes.Service{Spec: dockerswarmtypes.ServiceSpec{Annotations: dockerswarmtypes.Annotations{Name: "stack_web"}}}
	h := newStackActionTestHandler(t, []dockerswarmtypes.Service{service})

	result, output, err := h.controlStack(t.Context(), nil, controlStackInput{StackName: "stack", Action: "restart", Service: "missing"})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("result = %#v, output = %#v, error = %v", result, output, err)
	}

	if output.StackName != "stack" || output.Action != "restart" || len(output.Results) != 0 {
		t.Fatalf("structured output = %#v", output)
	}

	if len(result.Content) != 1 || !strings.Contains(result.Content[0].(*sdkmcp.TextContent).Text, "service not found: stack_missing") {
		t.Fatalf("content = %#v", result.Content)
	}
}

func TestControlStackReturnsPartialStructuredOutputOnHardError(t *testing.T) {
	services := []dockerswarmtypes.Service{
		{ID: "first-id", Spec: dockerswarmtypes.ServiceSpec{Annotations: dockerswarmtypes.Annotations{Name: "stack_first"}, Mode: dockerswarmtypes.ServiceMode{Replicated: &dockerswarmtypes.ReplicatedService{}}}},
		{ID: "second-id", Spec: dockerswarmtypes.ServiceSpec{Annotations: dockerswarmtypes.Annotations{Name: "stack_second"}, Mode: dockerswarmtypes.ServiceMode{Replicated: &dockerswarmtypes.ReplicatedService{}}}},
	}

	var inspectedThird bool

	h := newStackActionTestHandlerWithFailure(t, services, "stack_second", &inspectedThird)

	result, output, err := h.controlStack(t.Context(), nil, controlStackInput{StackName: "stack", Action: "restart"})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("result = %#v, output = %#v, error = %v", result, output, err)
	}

	if output.StackName != "stack" || output.Action != "restart" || len(output.Results) != 1 || output.Results[0] != (controlplane.StackActionResult{Service: "stack_first", Status: "ok"}) {
		t.Fatalf("structured output = %#v", output)
	}

	if len(result.Content) != 1 || !strings.Contains(result.Content[0].(*sdkmcp.TextContent).Text, "stack_second") || !strings.Contains(result.Content[0].(*sdkmcp.TextContent).Text, "forced inspect failure") {
		t.Fatalf("content = %#v", result.Content)
	}
}

func TestMCPSwarmToolsRuntimeBehavior(t *testing.T) {
	dockerCli, err := docker.CreateDockerCli(false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	contexts := docker.NewContextRegistry(dockerCli, docker.ContextRegistryOptions{SwarmFeatures: true})

	t.Cleanup(func() { _ = contexts.Close() })

	h := &Handler{dockerCli: dockerCli, contexts: contexts, log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	assertMCPToolError(t, session, "get_stack", map[string]any{}, "stack_name")
	assertMCPToolError(t, session, "get_stack", map[string]any{"stack_name": "  "}, "missing stack name")

	swarmModeEnabled, err := dockerswarm.ResolveModeEnabled(t.Context(), dockerCli.Client())
	if err != nil {
		t.Fatalf("failed to resolve Docker Swarm mode: %v", err)
	}

	if !swarmModeEnabled {
		assertMCPToolError(t, session, "list_stacks", map[string]any{}, "swarm")
		assertMCPToolError(t, session, "get_stack", map[string]any{"stack_name": "missing"}, "swarm")
		assertMCPToolError(t, session, "control_stack", map[string]any{"stack_name": "missing", "action": "restart"}, "swarm")
		assertMCPToolError(t, session, "remove_stack", map[string]any{"stack_name": "missing"}, "swarm")

		return
	}

	stackName := fmt.Sprintf("doco-cd-mcp-tools-%d", time.Now().UnixNano())
	serviceName := stackName + "_service"
	replicas := uint64(0)

	createdService, err := dockerCli.Client().ServiceCreate(t.Context(), dockerclient.ServiceCreateOptions{
		Spec: dockerswarmtypes.ServiceSpec{
			Annotations: dockerswarmtypes.Annotations{
				Name:   serviceName,
				Labels: map[string]string{dockerswarm.StackNamespaceLabel: stackName},
			},
			TaskTemplate: dockerswarmtypes.TaskSpec{
				ContainerSpec: &dockerswarmtypes.ContainerSpec{Image: "busybox:latest"},
			},
			Mode: dockerswarmtypes.ServiceMode{
				Replicated: &dockerswarmtypes.ReplicatedService{Replicas: &replicas},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to create Swarm service fixture: %v", err)
	}

	t.Cleanup(func() {
		_, _ = dockerCli.Client().ServiceRemove(context.Background(), createdService.ID, dockerclient.ServiceRemoveOptions{})
	})

	result := callMCPTool(t, session, "list_stacks", map[string]any{})

	var output listStacksOutput
	decodeMCPStructuredContent(t, result, &output)

	services, ok := output.Stacks[stackName]
	if !ok || len(services) != 1 || services[0].Name != serviceName {
		t.Fatalf("expected stack fixture %q, got %#v", stackName, output.Stacks)
	}

	stackResult := callMCPTool(t, session, "get_stack", map[string]any{"stack_name": stackName})

	var stackOutput getStackOutput
	decodeMCPStructuredContent(t, stackResult, &stackOutput)

	if len(stackOutput.Services) != 1 || stackOutput.Services[0].Name != serviceName {
		t.Fatalf("expected service fixture %q, got %#v", serviceName, stackOutput.Services)
	}
}

func TestMCPSwarmToolsDisabledAtRuntime(t *testing.T) {
	dockerCli, err := docker.CreateDockerCli(false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	h := &Handler{dockerCli: dockerCli, log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	assertMCPToolError(t, session, "list_stacks", map[string]any{}, "disabled")
	assertMCPToolError(t, session, "get_stack", map[string]any{"stack_name": "stack"}, "disabled")
	assertMCPToolError(t, session, "control_stack", map[string]any{"stack_name": "stack", "action": "restart"}, "disabled")
	assertMCPToolError(t, session, "remove_stack", map[string]any{"stack_name": "stack"}, "disabled")
}

func newStackActionTestHandler(t *testing.T, services []dockerswarmtypes.Service) *Handler {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info"):
			_, _ = w.Write([]byte(`{"Swarm":{"ControlAvailable":true}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/services"):
			_ = json.NewEncoder(w).Encode(services)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/services/"):
			serviceName := path.Base(r.URL.Path)
			for _, service := range services {
				if service.Spec.Name == serviceName {
					_ = json.NewEncoder(w).Encode(service)

					return
				}
			}

			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.52")

	dockerCli, err := docker.CreateDockerCli(true)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	contexts := docker.NewContextRegistry(dockerCli, docker.ContextRegistryOptions{SwarmFeatures: true})

	t.Cleanup(func() { _ = contexts.Close() })

	return &Handler{dockerCli: dockerCli, contexts: contexts, log: logger.New(logger.LevelCritical)}
}

func newStackActionTestHandlerWithFailure(
	t *testing.T,
	services []dockerswarmtypes.Service,
	failedService string,
	inspectedThird *bool,
) *Handler {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		serviceName := path.Base(r.URL.Path)

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info"):
			_, _ = w.Write([]byte(`{"Swarm":{"ControlAvailable":true}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/services"):
			_ = json.NewEncoder(w).Encode(services)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/services/"):
			if serviceName == "stack_third" {
				*inspectedThird = true
			}

			if serviceName == failedService {
				http.Error(w, "forced inspect failure", http.StatusInternalServerError)
				return
			}

			for _, service := range services {
				if service.Spec.Name == serviceName {
					_ = json.NewEncoder(w).Encode(service)
					return
				}
			}
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/update"):
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.52")

	dockerCli, err := docker.CreateDockerCli(true)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	contexts := docker.NewContextRegistry(dockerCli, docker.ContextRegistryOptions{SwarmFeatures: true})

	t.Cleanup(func() { _ = contexts.Close() })

	return &Handler{dockerCli: dockerCli, contexts: contexts, log: logger.New(logger.LevelCritical)}
}
