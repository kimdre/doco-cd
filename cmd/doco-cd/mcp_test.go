package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker"
	dockerswarm "github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	prometheusmetrics "github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/test"
)

const testMCPAPIKey = "test-mcp-api-key" // #nosec G101 -- test fixture, not a real credential.

const projectToolsComposeContent = `services:
  nginx:
    image: nginx:latest
    volumes:
      - data:/data
volumes:
  data:
`

type apiKeyRoundTripper struct {
	apiKey string
	base   http.RoundTripper
}

func (rt apiKeyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clonedRequest := req.Clone(req.Context())
	clonedRequest.Header.Set(restapi.KeyHeader, rt.apiKey)

	return rt.base.RoundTrip(clonedRequest)
}

func TestMCPServerAuthentication(t *testing.T) {
	server, _ := newMCPTestServer(t, true, testMCPAPIKey, 1024)

	for _, testCase := range []struct {
		name   string
		apiKey string
	}{
		{name: "missing API key"},
		{name: "wrong API key", apiKey: "wrong"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, server.URL+mcpPath, bytes.NewBufferString(`{}`))
			if err != nil {
				t.Fatal(err)
			}

			request.Header.Set("Content-Type", "application/json")

			if testCase.apiKey != "" {
				request.Header.Set(restapi.KeyHeader, testCase.apiKey)
			}

			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close() //nolint:errcheck

			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, response.StatusCode)
			}
		})
	}
}

func TestMCPServerRouteRegistration(t *testing.T) {
	t.Run("disabled route is absent", func(t *testing.T) {
		server, enabledEndpoints := newMCPTestServer(t, false, testMCPAPIKey, 1024)

		response, err := server.Client().Post(server.URL+mcpPath, "application/json", bytes.NewBufferString(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close() //nolint:errcheck

		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("expected status %d, got %d", http.StatusNotFound, response.StatusCode)
		}

		if containsEndpoint(enabledEndpoints, mcpPath) {
			t.Fatalf("disabled endpoint unexpectedly registered: %v", enabledEndpoints)
		}
	})

	t.Run("empty API secret leaves route absent", func(t *testing.T) {
		server, enabledEndpoints := newMCPTestServer(t, true, "", 1024)

		response, err := server.Client().Post(server.URL+mcpPath, "application/json", bytes.NewBufferString(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close() //nolint:errcheck

		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("expected status %d, got %d", http.StatusNotFound, response.StatusCode)
		}

		if containsEndpoint(enabledEndpoints, mcpPath) {
			t.Fatalf("endpoint without API secret unexpectedly registered: %v", enabledEndpoints)
		}
	})

	t.Run("enabled endpoint is reported and POST-only", func(t *testing.T) {
		server, enabledEndpoints := newMCPTestServer(t, true, testMCPAPIKey, 1024)

		if !containsEndpoint(enabledEndpoints, mcpPath) {
			t.Fatalf("enabled endpoint not reported: %v", enabledEndpoints)
		}

		request, err := http.NewRequest(http.MethodGet, server.URL+mcpPath, nil)
		if err != nil {
			t.Fatal(err)
		}

		request.Header.Set(restapi.KeyHeader, testMCPAPIKey)

		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close() //nolint:errcheck

		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, response.StatusCode)
		}
	})
}

func TestMCPServerListsTools(t *testing.T) {
	server, _ := newMCPTestServer(t, true, testMCPAPIKey, 1024)
	session := connectMCPTestClient(t, server)

	result, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Tools) != 10 {
		t.Fatalf("expected exactly ten MCP tools, got %#v", result.Tools)
	}

	wantTools := map[string]bool{
		"get_health":           false,
		"list_deployment_runs": false,
		"get_deployment_run":   false,
		"list_scheduled_jobs":  false,
		"list_projects":        false,
		"get_project":          false,
		"list_stacks":          false,
		"get_stack":            false,
		"control_project":      false,
		"destroy_project":      false,
	}
	wantInputProperties := map[string][]string{
		"list_deployment_runs": {"limit", "status", "trigger"},
		"get_deployment_run":   {"job_id"},
		"list_scheduled_jobs":  {"stack"},
		"list_projects":        {"all"},
		"get_project":          {"project_name"},
		"get_stack":            {"stack_name"},
		"control_project":      {"project_name", "action", "timeout"},
		"destroy_project":      {"project_name", "timeout", "volumes", "images"},
	}
	wantRequiredProperties := map[string][]string{
		"control_project": {"project_name", "action"},
		"destroy_project": {"project_name"},
	}

	for _, tool := range result.Tools {
		if _, ok := wantTools[tool.Name]; !ok {
			t.Fatalf("unexpected tool %q", tool.Name)
		}

		wantTools[tool.Name] = true
		if tool.Annotations == nil {
			t.Fatalf("%s must have annotations", tool.Name)
		}

		if tool.Name != "control_project" && tool.Name != "destroy_project" && !tool.Annotations.ReadOnlyHint {
			t.Fatalf("%s must have readOnlyHint=true: %#v", tool.Name, tool.Annotations)
		}

		if tool.Name == "get_project" && !strings.Contains(tool.Description, "returns the project's containers") {
			t.Fatalf("get_project description must say it returns the project's containers: %q", tool.Description)
		}

		if tool.Name == "list_deployment_runs" {
			limitSchema := toolSchemaProperty(t, tool.InputSchema, "limit")
			if minimum, ok := limitSchema["minimum"].(float64); !ok || minimum != 1 {
				t.Fatalf("list_deployment_runs limit minimum = %#v, want 1", limitSchema["minimum"])
			}

			if _, ok := limitSchema["maximum"]; ok {
				t.Fatalf("list_deployment_runs limit schema must not set maximum: %#v", limitSchema)
			}
		}

		if tool.Name == "control_project" {
			assertMCPProjectToolAnnotations(t, tool, true, false)

			actionSchema := toolSchemaProperty(t, tool.InputSchema, "action")
			if !slices.Equal(actionSchema["enum"].([]any), []any{"start", "stop", "restart"}) {
				t.Fatalf("control_project action enum = %#v", actionSchema["enum"])
			}
		}

		if tool.Name == "destroy_project" {
			assertMCPProjectToolAnnotations(t, tool, true, true)

			if !strings.Contains(strings.ToLower(tool.Description), "restored automatically") {
				t.Fatalf("destroy_project description must warn about reconciliation: %q", tool.Description)
			}
		}

		for _, property := range wantInputProperties[tool.Name] {
			if !toolSchemaHasProperty(tool.InputSchema, property) {
				t.Fatalf("%s input schema must contain snake_case property %q: %#v", tool.Name, property, tool.InputSchema)
			}
		}

		for _, property := range wantRequiredProperties[tool.Name] {
			if !toolSchemaRequiresProperty(tool.InputSchema, property) {
				t.Fatalf("%s input schema must require %q: %#v", tool.Name, property, tool.InputSchema)
			}
		}
	}

	for name, found := range wantTools {
		if !found {
			t.Fatalf("expected tool %q to be registered", name)
		}
	}
}

func TestMCPProjectToolValidation(t *testing.T) {
	h := &handlerData{appConfig: &app.Config{}, appVersion: app.Version, log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	for _, testCase := range []struct {
		name      string
		tool      string
		arguments map[string]any
		contains  string
	}{
		{name: "control missing project", tool: "control_project", arguments: map[string]any{"action": "start"}, contains: "project_name"},
		{name: "control blank project", tool: "control_project", arguments: map[string]any{"project_name": "  ", "action": "start"}, contains: "missing project name"},
		{name: "control missing action", tool: "control_project", arguments: map[string]any{"project_name": "project"}, contains: "action"},
		{name: "control invalid action", tool: "control_project", arguments: map[string]any{"project_name": "project", "action": "invalid"}, contains: "not in enum"},
		{name: "control invalid timeout", tool: "control_project", arguments: map[string]any{"project_name": "project", "action": "start", "timeout": "invalid"}, contains: "timeout"},
		{name: "control nil docker", tool: "control_project", arguments: map[string]any{"project_name": "project", "action": "start"}, contains: "docker cli is required"},
		{name: "destroy missing project", tool: "destroy_project", arguments: map[string]any{}, contains: "project_name"},
		{name: "destroy blank project", tool: "destroy_project", arguments: map[string]any{"project_name": "  "}, contains: "missing project name"},
		{name: "destroy invalid timeout", tool: "destroy_project", arguments: map[string]any{"project_name": "project", "timeout": "invalid"}, contains: "timeout"},
		{name: "destroy nil docker", tool: "destroy_project", arguments: map[string]any{"project_name": "project"}, contains: "docker cli is required"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			assertMCPToolError(t, session, testCase.tool, testCase.arguments, testCase.contains)
		})
	}
}

func TestMCPProjectTools(t *testing.T) {
	skipWithoutLiveDocker(t)

	dockerCli, err := docker.CreateDockerCli(false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	if dockerswarm.GetModeEnabled() {
		t.Skip("compose project tools require standalone mode")
	}

	projectName := test.ConvertTestName(t.Name())
	test.ComposeUp(t.Context(), t, test.WithYAML(projectToolsComposeContent), test.WithName(projectName))

	h := &handlerData{dockerCli: dockerCli, appConfig: &app.Config{}, appVersion: app.Version, log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	for _, action := range []string{"stop", "start", "restart"} {
		result := callMCPTool(t, session, "control_project", map[string]any{
			"project_name": projectName,
			"action":       action,
			"timeout":      30,
		})

		var output controlProjectOutput
		decodeMCPStructuredContent(t, result, &output)

		if output.ProjectName != projectName || output.Action != action || output.Status != "completed" {
			t.Fatalf("unexpected %s output: %#v", action, output)
		}

		wantState := "running"
		if action == "stop" {
			wantState = "exited"
		}

		waitForProjectState(t, dockerCli, projectName, wantState)
	}

	assertMCPToolError(t, session, "control_project", map[string]any{
		"project_name": "missing",
		"action":       "stop",
	}, "project not found")

	result := callMCPTool(t, session, "destroy_project", map[string]any{
		"project_name": projectName,
		"timeout":      30,
		"volumes":      true,
		"images":       false,
	})

	var output destroyProjectOutput
	decodeMCPStructuredContent(t, result, &output)

	if output.ProjectName != projectName || output.Status != "destroyed" || !output.Volumes || output.Images {
		t.Fatalf("unexpected destroy output: %#v", output)
	}

	containers, err := docker.GetProjectContainers(t.Context(), dockerCli, projectName)
	if err != nil {
		t.Fatal(err)
	}

	if len(containers) != 0 {
		t.Fatalf("project still has containers after destroy: %#v", containers)
	}

	volumes, err := docker.GetLabeledVolumes(t.Context(), dockerCli.Client(), api.ProjectLabel, projectName)
	if err != nil {
		t.Fatal(err)
	}

	if len(volumes) != 0 {
		t.Fatalf("project still has volumes after destroy: %#v", volumes)
	}
}

func TestMCPDeploymentRunTools(t *testing.T) {
	tracker := newDeploymentRunTracker(nil)
	tracker.TrackAccepted("job-webhook", deploymentRunTriggerWebhook)
	tracker.MarkSucceeded("job-webhook", "complete")
	tracker.TrackAccepted("job-poll", deploymentRunTriggerPoll)

	h := &handlerData{runTracker: tracker, appConfig: &app.Config{}, appVersion: app.Version, log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	t.Run("list defaults and normalized filters", func(t *testing.T) {
		result := callMCPTool(t, session, "list_deployment_runs", map[string]any{
			"limit":   1,
			"status":  " SUCCEEDED ",
			"trigger": " WEBHOOK ",
		})

		var output listDeploymentRunsOutput
		decodeMCPStructuredContent(t, result, &output)

		if len(output.Runs) != 1 || output.Runs[0].JobID != "job-webhook" {
			t.Fatalf("unexpected filtered runs: %#v", output.Runs)
		}
	})

	t.Run("list defaults to 50", func(t *testing.T) {
		defaultTracker := newDeploymentRunTracker(map[deploymentRunTrigger]int{deploymentRunTriggerWebhook: 60})
		for i := range 60 {
			defaultTracker.TrackAccepted("default-job-"+strconv.Itoa(i), deploymentRunTriggerWebhook)
		}

		defaultHandler := &handlerData{runTracker: defaultTracker, appConfig: &app.Config{}, appVersion: app.Version, log: logger.New(logger.LevelCritical)}
		defaultServer, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, defaultHandler)
		result := callMCPTool(t, connectMCPTestClient(t, defaultServer), "list_deployment_runs", map[string]any{})

		var output listDeploymentRunsOutput
		decodeMCPStructuredContent(t, result, &output)

		if len(output.Runs) != 50 {
			t.Fatalf("expected default limit 50, got %d runs", len(output.Runs))
		}
	})

	t.Run("get run", func(t *testing.T) {
		result := callMCPTool(t, session, "get_deployment_run", map[string]any{"job_id": " job-webhook "})

		var output getDeploymentRunOutput
		decodeMCPStructuredContent(t, result, &output)

		if output.Run.JobID != "job-webhook" || output.Run.Status != deploymentRunStatusSucceeded {
			t.Fatalf("unexpected run: %#v", output.Run)
		}
	})

	for _, testCase := range []struct {
		name      string
		tool      string
		arguments map[string]any
		contains  string
	}{
		{name: "zero limit", tool: "list_deployment_runs", arguments: map[string]any{"limit": 0}, contains: "less than 1"},
		{name: "negative limit", tool: "list_deployment_runs", arguments: map[string]any{"limit": -1}, contains: "less than 1"},
		{name: "invalid status", tool: "list_deployment_runs", arguments: map[string]any{"status": "unknown"}, contains: "invalid deployment run status"},
		{name: "invalid trigger", tool: "list_deployment_runs", arguments: map[string]any{"trigger": "unknown"}, contains: "invalid deployment run trigger"},
		{name: "missing job id", tool: "get_deployment_run", arguments: map[string]any{}, contains: "job_id"},
		{name: "blank job id", tool: "get_deployment_run", arguments: map[string]any{"job_id": "  "}, contains: "missing job id"},
		{name: "run not found", tool: "get_deployment_run", arguments: map[string]any{"job_id": "missing"}, contains: "run not found: missing"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			assertMCPToolError(t, session, testCase.tool, testCase.arguments, testCase.contains)
		})
	}
}

func TestMCPListDeploymentRunsCapsLimitAt200(t *testing.T) {
	tracker := newDeploymentRunTracker(map[deploymentRunTrigger]int{
		deploymentRunTriggerWebhook: 250,
	})
	for i := range 205 {
		tracker.TrackAccepted("job-"+strconv.Itoa(i), deploymentRunTriggerWebhook)
	}

	h := &handlerData{runTracker: tracker, appConfig: &app.Config{}, appVersion: app.Version, log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	result := callMCPTool(t, connectMCPTestClient(t, server), "list_deployment_runs", map[string]any{"limit": 201})

	var output listDeploymentRunsOutput
	decodeMCPStructuredContent(t, result, &output)

	if len(output.Runs) != 200 {
		t.Fatalf("expected limit to be capped at 200, got %d runs", len(output.Runs))
	}
}

func TestMCPDockerReadTools(t *testing.T) {
	skipWithoutLiveDocker(t)

	dockerCli, err := docker.CreateDockerCli(false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	if dockerswarm.GetModeEnabled() {
		t.Skip("compose project tools require standalone mode")
	}

	projectName := test.ConvertTestName(t.Name())
	test.ComposeUp(t.Context(), t, test.WithYAML(composeContent), test.WithName(projectName))

	h := &handlerData{dockerCli: dockerCli, appConfig: &app.Config{}, appVersion: app.Version, log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	t.Run("list projects", func(t *testing.T) {
		result := callMCPTool(t, session, "list_projects", map[string]any{"all": true})

		var output listProjectsOutput
		decodeMCPStructuredContent(t, result, &output)

		if !containsProject(output.Projects, projectName) {
			t.Fatalf("expected project %q in %#v", projectName, output.Projects)
		}
	})

	t.Run("get project containers", func(t *testing.T) {
		result := callMCPTool(t, session, "get_project", map[string]any{"project_name": projectName})

		var output getProjectOutput
		decodeMCPStructuredContent(t, result, &output)

		if len(output.Containers) == 0 {
			t.Fatalf("expected containers for project %q", projectName)
		}
	})

	assertMCPToolError(t, session, "get_project", map[string]any{"project_name": "missing"}, "project not found")
}

func TestMCPGetProjectRequiresName(t *testing.T) {
	h := &handlerData{appConfig: &app.Config{}, appVersion: app.Version, log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	assertMCPToolError(t, session, "get_project", map[string]any{}, "project_name")
	assertMCPToolError(t, session, "get_project", map[string]any{"project_name": "  "}, "missing project name")
}

func TestMCPDockerReadToolsRequireDockerCLI(t *testing.T) {
	h := &handlerData{appConfig: &app.Config{}, appVersion: app.Version, log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	assertMCPToolError(t, session, "list_scheduled_jobs", map[string]any{}, "docker cli is required")
	assertMCPToolError(t, session, "list_projects", map[string]any{}, "docker cli is required")
	assertMCPToolError(t, session, "get_project", map[string]any{"project_name": "project"}, "docker cli is required")
}

func TestMCPScheduledJobsTool(t *testing.T) {
	skipWithoutLiveDocker(t)

	dockerCli, err := docker.CreateDockerCli(false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	if dockerswarm.GetModeEnabled() {
		t.Skip("compose scheduled-job fixture requires standalone mode")
	}

	projectName := test.ConvertTestName(t.Name())
	test.ComposeUp(t.Context(), t, test.WithYAML(composeContent), test.WithName(projectName))

	h := &handlerData{dockerCli: dockerCli, appConfig: &app.Config{}, appVersion: app.Version, log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)
	result := callMCPTool(t, session, "list_scheduled_jobs", map[string]any{"stack": " " + projectName + " "})

	var output listScheduledJobsOutput
	decodeMCPStructuredContent(t, result, &output)

	if !containsScheduledJob(output.Jobs, projectName) {
		t.Fatalf("expected scheduled job for stack %q in %#v", projectName, output.Jobs)
	}
}

func TestMCPSwarmToolsRuntimeBehavior(t *testing.T) {
	dockerCli, err := docker.CreateDockerCli(false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	h := &handlerData{dockerCli: dockerCli, appConfig: &app.Config{}, appVersion: app.Version, log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	assertMCPToolError(t, session, "get_stack", map[string]any{}, "stack_name")
	assertMCPToolError(t, session, "get_stack", map[string]any{"stack_name": "  "}, "missing stack name")

	if !dockerswarm.GetModeEnabled() {
		assertMCPToolError(t, session, "list_stacks", map[string]any{}, "swarm")
		assertMCPToolError(t, session, "get_stack", map[string]any{"stack_name": "missing"}, "swarm")

		return
	}

	result := callMCPTool(t, session, "list_stacks", map[string]any{})

	var output listStacksOutput
	decodeMCPStructuredContent(t, result, &output)

	for stackName := range output.Stacks {
		stackResult := callMCPTool(t, session, "get_stack", map[string]any{"stack_name": stackName})

		var stackOutput getStackOutput
		decodeMCPStructuredContent(t, stackResult, &stackOutput)

		if len(stackOutput.Services) == 0 {
			t.Fatalf("expected services for stack %q", stackName)
		}

		return
	}

	t.Log("swarm is active, but no stack fixture is available for get_stack success coverage")
}

func TestMCPSwarmToolsDisabledAtRuntime(t *testing.T) {
	dockerCli, err := docker.CreateDockerCli(false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	previousDisableSwarmFeature := dockerswarm.GetDisableSwarmFeature()

	dockerswarm.SetDisableSwarmFeature(true)
	t.Cleanup(func() { dockerswarm.SetDisableSwarmFeature(previousDisableSwarmFeature) })

	h := &handlerData{dockerCli: dockerCli, appConfig: &app.Config{}, appVersion: app.Version, log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	assertMCPToolError(t, session, "list_stacks", map[string]any{}, "disabled")
	assertMCPToolError(t, session, "get_stack", map[string]any{"stack_name": "stack"}, "disabled")
}

func TestMCPServerGetHealth(t *testing.T) {
	server, _ := newMCPTestServer(t, true, testMCPAPIKey, 1024)
	session := connectMCPTestClient(t, server)
	requestsBefore := testutil.ToFloat64(prometheusmetrics.McpRequestsTotal.WithLabelValues("get_health"))
	errorsBefore := testutil.ToFloat64(prometheusmetrics.McpErrorsTotal.WithLabelValues("get_health"))
	durationsBefore := histogramSampleCount(t, prometheusmetrics.McpRequestDuration.WithLabelValues("get_health"))

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_health", Arguments: struct{}{}})
	if err != nil {
		t.Fatal(err)
	}

	if result.IsError {
		t.Fatalf("get_health returned a tool error: %#v", result.Content)
	}

	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected structured object, got %T", result.StructuredContent)
	}

	if structured["status"] != "healthy" {
		t.Fatalf("expected healthy status, got %#v", structured)
	}

	requestsAfter := testutil.ToFloat64(prometheusmetrics.McpRequestsTotal.WithLabelValues("get_health"))
	errorsAfter := testutil.ToFloat64(prometheusmetrics.McpErrorsTotal.WithLabelValues("get_health"))
	durationsAfter := histogramSampleCount(t, prometheusmetrics.McpRequestDuration.WithLabelValues("get_health"))

	if requestsAfter-requestsBefore != 1 {
		t.Fatalf("expected one instrumented call, got delta %v", requestsAfter-requestsBefore)
	}

	if errorsAfter != errorsBefore {
		t.Fatalf("expected no error metric increment, got delta %v", errorsAfter-errorsBefore)
	}

	if durationsAfter-durationsBefore != 1 {
		t.Fatalf("expected one duration observation, got delta %d", durationsAfter-durationsBefore)
	}
}

func TestMCPServerGetHealthFailure(t *testing.T) {
	dockerAPIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(dockerAPIServer.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(dockerAPIServer.URL, "http://"))

	server, _ := newMCPTestServer(t, true, testMCPAPIKey, 1024)
	session := connectMCPTestClient(t, server)
	errorsBefore := testutil.ToFloat64(prometheusmetrics.McpErrorsTotal.WithLabelValues("get_health"))

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_health", Arguments: struct{}{}})
	if err != nil {
		t.Fatal(err)
	}

	if !result.IsError {
		t.Fatalf("expected get_health tool error, got %#v", result)
	}

	errorsAfter := testutil.ToFloat64(prometheusmetrics.McpErrorsTotal.WithLabelValues("get_health"))
	if errorsAfter-errorsBefore != 1 {
		t.Fatalf("expected one error metric increment, got delta %v", errorsAfter-errorsBefore)
	}
}

func TestMCPServerRejectsOversizedBody(t *testing.T) {
	server, _ := newMCPTestServer(t, true, testMCPAPIKey, 32)

	request, err := http.NewRequest(http.MethodPost, server.URL+mcpPath, bytes.NewReader(make([]byte, 33)))
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set(restapi.KeyHeader, testMCPAPIKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")

	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck

	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status %d, got %d", http.StatusRequestEntityTooLarge, response.StatusCode)
	}
}

func TestInstrumentMCPToolCountsResultErrors(t *testing.T) {
	const toolName = "test_result_error"

	requestsBefore := testutil.ToFloat64(prometheusmetrics.McpRequestsTotal.WithLabelValues(toolName))
	errorsBefore := testutil.ToFloat64(prometheusmetrics.McpErrorsTotal.WithLabelValues(toolName))
	handler := instrumentMCPTool(logger.New(logger.LevelCritical), toolName, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return &mcp.CallToolResult{IsError: true}, struct{}{}, nil
	})

	result, _, err := handler(t.Context(), nil, struct{}{})
	if err != nil {
		t.Fatal(err)
	}

	if result == nil || !result.IsError {
		t.Fatalf("expected MCP tool error result, got %#v", result)
	}

	requestsAfter := testutil.ToFloat64(prometheusmetrics.McpRequestsTotal.WithLabelValues(toolName))
	errorsAfter := testutil.ToFloat64(prometheusmetrics.McpErrorsTotal.WithLabelValues(toolName))

	if requestsAfter-requestsBefore != 1 {
		t.Fatalf("expected one call, got delta %v", requestsAfter-requestsBefore)
	}

	if errorsAfter-errorsBefore != 1 {
		t.Fatalf("expected one error, got delta %v", errorsAfter-errorsBefore)
	}
}

func TestInstrumentMCPToolCountsHandlerErrorsOnce(t *testing.T) {
	const toolName = "test_handler_error"

	requestsBefore := testutil.ToFloat64(prometheusmetrics.McpRequestsTotal.WithLabelValues(toolName))
	errorsBefore := testutil.ToFloat64(prometheusmetrics.McpErrorsTotal.WithLabelValues(toolName))
	handler := instrumentMCPTool(logger.New(logger.LevelCritical), toolName, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return &mcp.CallToolResult{IsError: true}, struct{}{}, errors.New("failed")
	})

	_, _, err := handler(t.Context(), nil, struct{}{})
	if err == nil {
		t.Fatal("expected handler error")
	}

	requestsAfter := testutil.ToFloat64(prometheusmetrics.McpRequestsTotal.WithLabelValues(toolName))
	errorsAfter := testutil.ToFloat64(prometheusmetrics.McpErrorsTotal.WithLabelValues(toolName))

	if requestsAfter-requestsBefore != 1 {
		t.Fatalf("expected one call, got delta %v", requestsAfter-requestsBefore)
	}

	if errorsAfter-errorsBefore != 1 {
		t.Fatalf("expected one error without double counting, got delta %v", errorsAfter-errorsBefore)
	}
}

func TestInstrumentMCPToolLogsHandlerFailure(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		result     *mcp.CallToolResult
		err        error
		wantDetail string
	}{
		{name: "handler error", result: &mcp.CallToolResult{IsError: true}, err: errors.New("handler failed"), wantDetail: "handler failed"},
		{name: "error result", result: &mcp.CallToolResult{IsError: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var logOutput bytes.Buffer

			log := &logger.Logger{
				Logger: slog.New(slog.NewTextHandler(&logOutput, nil)),
				Level:  slog.LevelDebug,
			}
			handler := instrumentMCPTool(log, "test_failure", func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
				return testCase.result, struct{}{}, testCase.err
			})

			_, _, _ = handler(t.Context(), nil, struct{}{})

			logged := logOutput.String()
			if !strings.Contains(logged, "tool=test_failure") || testCase.wantDetail != "" && !strings.Contains(logged, testCase.wantDetail) {
				t.Fatalf("expected tool failure in log, got %q", logged)
			}

			if count := strings.Count(logged, "MCP tool call failed"); count != 1 {
				t.Fatalf("expected one failure log entry, got %d in %q", count, logged)
			}
		})
	}
}

func newMCPTestServer(t *testing.T, enabled bool, apiSecret string, maxPayloadSize int64) (*httptest.Server, []string) {
	t.Helper()

	h := &handlerData{appConfig: &app.Config{}, appVersion: app.Version, log: logger.New(logger.LevelCritical)}

	return newMCPTestServerWithHandler(t, enabled, apiSecret, maxPayloadSize, h)
}

func newMCPTestServerWithHandler(t *testing.T, enabled bool, apiSecret string, maxPayloadSize int64, handler *handlerData) (*httptest.Server, []string) {
	t.Helper()

	config := &app.Config{
		ApiSecret:      apiSecret,
		McpEnabled:     enabled,
		MaxPayloadSize: maxPayloadSize,
	}
	log := handler.log
	mux := http.NewServeMux()
	enabledEndpoints := registerApiEndpoints(config, handler, log, mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server, enabledEndpoints
}

func callMCPTool(t *testing.T, session *mcp.ClientSession, name string, arguments any) *mcp.CallToolResult {
	t.Helper()

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}

	if result.IsError {
		t.Fatalf("%s returned a tool error: %#v", name, result.Content)
	}

	return result
}

func assertMCPToolError(t *testing.T, session *mcp.ClientSession, name string, arguments any, contains string) {
	t.Helper()

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}

	if !result.IsError {
		t.Fatalf("expected %s tool error, got %#v", name, result)
	}

	encoded, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(contains)) {
		t.Fatalf("expected %s error to contain %q, got %s", name, contains, encoded)
	}
}

func decodeMCPStructuredContent(t *testing.T, result *mcp.CallToolResult, output any) {
	t.Helper()

	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(encoded, output); err != nil {
		t.Fatalf("decode structured content %s: %v", encoded, err)
	}
}

func containsProject(projects []api.Stack, name string) bool {
	for _, project := range projects {
		if project.Name == name {
			return true
		}
	}

	return false
}

func containsScheduledJob(jobs []scheduler.JobInfo, stack string) bool {
	for _, job := range jobs {
		if job.Stack == stack {
			return true
		}
	}

	return false
}

func toolSchemaHasProperty(schema any, property string) bool {
	schemaMap, ok := schema.(map[string]any)
	if !ok {
		return false
	}

	properties, ok := schemaMap["properties"].(map[string]any)
	if !ok {
		return false
	}

	_, ok = properties[property]

	return ok
}

func toolSchemaRequiresProperty(schema any, property string) bool {
	schemaMap, ok := schema.(map[string]any)
	if !ok {
		return false
	}

	required, ok := schemaMap["required"].([]any)
	if !ok {
		return false
	}

	return slices.Contains(required, any(property))
}

func toolSchemaProperty(t *testing.T, schema any, property string) map[string]any {
	t.Helper()

	schemaMap, ok := schema.(map[string]any)
	if !ok {
		t.Fatalf("expected object schema, got %T", schema)
	}

	properties, ok := schemaMap["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected schema properties, got %#v", schemaMap["properties"])
	}

	propertySchema, ok := properties[property].(map[string]any)
	if !ok {
		t.Fatalf("expected schema for property %q, got %#v", property, properties[property])
	}

	return propertySchema
}

func assertMCPProjectToolAnnotations(t *testing.T, tool *mcp.Tool, destructive, idempotent bool) {
	t.Helper()

	annotations := tool.Annotations
	if annotations.ReadOnlyHint || annotations.IdempotentHint != idempotent {
		t.Fatalf("%s annotations = %#v", tool.Name, annotations)
	}

	if annotations.DestructiveHint == nil || *annotations.DestructiveHint != destructive {
		t.Fatalf("%s destructiveHint = %#v, want %t", tool.Name, annotations.DestructiveHint, destructive)
	}

	if annotations.OpenWorldHint == nil || *annotations.OpenWorldHint {
		t.Fatalf("%s openWorldHint = %#v, want false", tool.Name, annotations.OpenWorldHint)
	}
}

func waitForProjectState(t *testing.T, dockerCli command.Cli, projectName, want string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		containers, err := docker.GetProjectContainers(t.Context(), dockerCli, projectName)
		if err == nil && len(containers) > 0 && strings.EqualFold(string(containers[0].State), want) {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	containers, err := docker.GetProjectContainers(t.Context(), dockerCli, projectName)
	if err != nil {
		t.Fatal(err)
	}

	t.Fatalf("project %q state = %#v, want %q", projectName, containers, want)
}

func skipWithoutLiveDocker(t *testing.T) {
	t.Helper()

	if os.Getenv("DOCO_CD_TEST_FAKE_DOCKER") != "" {
		t.Skip("requires a live Docker daemon")
	}
}

func connectMCPTestClient(t *testing.T, server *httptest.Server) *mcp.ClientSession {
	t.Helper()

	httpClient := server.Client()
	httpClient.Transport = apiKeyRoundTripper{apiKey: testMCPAPIKey, base: httpClient.Transport}
	client := mcp.NewClient(&mcp.Implementation{Name: "doco-cd-test", Version: "test"}, nil)

	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             server.URL + mcpPath,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close MCP session: %v", err)
		}
	})

	return session
}

func containsEndpoint(endpoints []string, target string) bool {
	return slices.Contains(endpoints, target)
}

func histogramSampleCount(t *testing.T, observer prometheus.Observer) uint64 {
	t.Helper()

	metric, ok := observer.(prometheus.Metric)
	if !ok {
		t.Fatalf("expected histogram observer to implement prometheus.Metric, got %T", observer)
	}

	dtoMetric := &dto.Metric{}
	if err := metric.Write(dtoMetric); err != nil {
		t.Fatalf("write histogram metric: %v", err)
	}

	return dtoMetric.GetHistogram().GetSampleCount()
}
