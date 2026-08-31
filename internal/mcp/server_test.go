package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/docker/cli/cli/command"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	prometheusmetrics "github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/restapi"
)

const testMCPAPIKey = "test-mcp-api-key" // #nosec G101 -- test fixture, not a real credential.

func TestNewHandlerValidatesDependencies(t *testing.T) {
	dockerCli, err := command.NewDockerCli()
	if err != nil {
		t.Fatal(err)
	}

	valid := Dependencies{
		Version:        "test",
		APISecret:      testMCPAPIKey,
		MaxPayloadSize: 1024,
		Logger:         logger.New(logger.LevelCritical),
		DockerCLI:      dockerCli,
		Contexts:       &docker.ContextRegistry{},
		Runs:           newTestControlPlaneRuns(t, testControlPlaneRunsOptions{}),
	}

	for _, testCase := range []struct {
		name     string
		contains string
		mutate   func(*Dependencies)
	}{
		{name: "version", contains: "Dependencies.Version", mutate: func(deps *Dependencies) { deps.Version = "" }},
		{name: "API secret", contains: "Dependencies.APISecret", mutate: func(deps *Dependencies) { deps.APISecret = "" }},
		{name: "payload size", contains: "Dependencies.MaxPayloadSize", mutate: func(deps *Dependencies) { deps.MaxPayloadSize = 0 }},
		{name: "logger", contains: "Dependencies.Logger", mutate: func(deps *Dependencies) { deps.Logger = nil }},
		{name: "logger backend", contains: "logger is required", mutate: func(deps *Dependencies) { deps.Logger = &logger.Logger{} }},
		{name: "docker CLI", contains: "Dependencies.DockerCLI", mutate: func(deps *Dependencies) { deps.DockerCLI = nil }},
		{name: "contexts", contains: "Dependencies.Contexts", mutate: func(deps *Dependencies) { deps.Contexts = nil }},
		{name: "runs", contains: "Dependencies.Runs", mutate: func(deps *Dependencies) { deps.Runs = nil }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dependencies := valid
			testCase.mutate(&dependencies)

			_, err := NewHandler(dependencies)
			if err == nil || !strings.Contains(err.Error(), testCase.contains) {
				t.Fatalf("NewHandler() error = %v, want containing %q", err, testCase.contains)
			}
		})
	}

	valid.TrustedProxyHeader = ""

	handler, err := NewHandler(valid)
	if err != nil {
		t.Fatalf("NewHandler() default proxy header: %v", err)
	}

	if handler.trustedProxyHeader != "X-Forwarded-For" {
		t.Fatalf("trusted proxy header = %q, want default", handler.trustedProxyHeader)
	}
}

func TestHandlerRequestIPUsesRESTAPIResolver(t *testing.T) {
	h := &Handler{
		trustedProxyHeader:   "X-Forwarded-For",
		trustedProxyNetworks: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	}
	request := httptest.NewRequest(http.MethodPost, testMCPPath, nil)
	request.RemoteAddr = "10.0.0.2:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.1")

	if got := h.requestIP(request); got != "198.51.100.7:1234" {
		t.Fatalf("requestIP() = %q, want %q", got, "198.51.100.7:1234")
	}
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
			request, err := http.NewRequest(http.MethodPost, server.URL+testMCPPath, bytes.NewBufferString(`{}`))
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

		response, err := server.Client().Post(server.URL+testMCPPath, "application/json", bytes.NewBufferString(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close() //nolint:errcheck

		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("expected status %d, got %d", http.StatusNotFound, response.StatusCode)
		}

		if containsEndpoint(enabledEndpoints, testMCPPath) {
			t.Fatalf("disabled endpoint unexpectedly registered: %v", enabledEndpoints)
		}
	})

	t.Run("empty API secret leaves route absent", func(t *testing.T) {
		server, enabledEndpoints := newMCPTestServer(t, true, "", 1024)

		response, err := server.Client().Post(server.URL+testMCPPath, "application/json", bytes.NewBufferString(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close() //nolint:errcheck

		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("expected status %d, got %d", http.StatusNotFound, response.StatusCode)
		}

		if containsEndpoint(enabledEndpoints, testMCPPath) {
			t.Fatalf("endpoint without API secret unexpectedly registered: %v", enabledEndpoints)
		}
	})

	t.Run("enabled endpoint is reported and POST-only", func(t *testing.T) {
		server, enabledEndpoints := newMCPTestServer(t, true, testMCPAPIKey, 1024)

		if !containsEndpoint(enabledEndpoints, testMCPPath) {
			t.Fatalf("enabled endpoint not reported: %v", enabledEndpoints)
		}

		request, err := http.NewRequest(http.MethodGet, server.URL+testMCPPath, nil)
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

	if len(result.Tools) != 14 {
		t.Fatalf("expected exactly fourteen MCP tools, got %#v", result.Tools)
	}

	wantTools := map[string]bool{
		"get_health":            false,
		"list_deployment_runs":  false,
		"get_deployment_run":    false,
		"list_scheduled_jobs":   false,
		"list_projects":         false,
		"get_project":           false,
		"list_stacks":           false,
		"get_stack":             false,
		"control_project":       false,
		"destroy_project":       false,
		"control_stack":         false,
		"remove_stack":          false,
		"trigger_scheduled_job": false,
		"trigger_poll":          false,
	}
	wantInputProperties := map[string][]string{
		"list_deployment_runs":  {"limit", "status", "trigger"},
		"get_deployment_run":    {"job_id"},
		"list_scheduled_jobs":   {"stack", "context"},
		"list_projects":         {"all", "context"},
		"get_project":           {"project_name", "context"},
		"list_stacks":           {"context"},
		"get_stack":             {"stack_name", "context"},
		"control_project":       {"project_name", "action", "timeout", "context"},
		"destroy_project":       {"project_name", "timeout", "volumes", "images", "context"},
		"control_stack":         {"stack_name", "action", "replicas", "service", "wait", "context"},
		"remove_stack":          {"stack_name", "context"},
		"trigger_scheduled_job": {"job_name", "stack", "wait", "context"},
		"trigger_poll":          {"configs", "wait"},
	}
	wantRequiredProperties := map[string][]string{
		"control_project":       {"project_name", "action"},
		"destroy_project":       {"project_name"},
		"control_stack":         {"stack_name", "action"},
		"remove_stack":          {"stack_name"},
		"trigger_scheduled_job": {"job_name"},
		"trigger_poll":          {"configs"},
	}

	for _, tool := range result.Tools {
		if _, ok := wantTools[tool.Name]; !ok {
			t.Fatalf("unexpected tool %q", tool.Name)
		}

		wantTools[tool.Name] = true
		if tool.Annotations == nil {
			t.Fatalf("%s must have annotations", tool.Name)
		}

		if tool.Name != "control_project" && tool.Name != "destroy_project" && tool.Name != "control_stack" && tool.Name != "remove_stack" && tool.Name != "trigger_scheduled_job" && tool.Name != "trigger_poll" && !tool.Annotations.ReadOnlyHint {
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
			assertMCPProjectTimeoutSchema(t, tool)

			actionSchema := toolSchemaProperty(t, tool.InputSchema, "action")
			if !slices.Equal(actionSchema["enum"].([]any), []any{"start", "stop", "restart"}) {
				t.Fatalf("control_project action enum = %#v", actionSchema["enum"])
			}
		}

		if tool.Name == "destroy_project" {
			assertMCPProjectToolAnnotations(t, tool, true, true)
			assertMCPProjectTimeoutSchema(t, tool)

			description := strings.ToLower(tool.Description)
			if !strings.Contains(description, "restored automatically") || !strings.Contains(description, "drift recovery") {
				t.Fatalf("destroy_project description must warn about reconciliation drift recovery: %q", tool.Description)
			}
		}

		if tool.Name == "control_stack" {
			assertMCPProjectToolAnnotations(t, tool, true, false)

			actionSchema := toolSchemaProperty(t, tool.InputSchema, "action")
			if !slices.Equal(actionSchema["enum"].([]any), []any{"scale", "restart", "run"}) {
				t.Fatalf("control_stack action enum = %#v", actionSchema["enum"])
			}

			replicasSchema := toolSchemaProperty(t, tool.InputSchema, "replicas")
			if replicasSchema["minimum"] != float64(0) {
				t.Fatalf("control_stack replicas minimum = %#v, want 0", replicasSchema["minimum"])
			}
		}

		if tool.Name == "remove_stack" {
			assertMCPProjectToolAnnotations(t, tool, true, true)
		}

		if tool.Name == "trigger_scheduled_job" {
			assertMCPProjectToolAnnotations(t, tool, true, false)

			if !strings.Contains(tool.Description, "Prefer wait=false and poll get_deployment_run") || !strings.Contains(tool.Description, "10s grace") {
				t.Fatalf("trigger_scheduled_job description lacks wait guidance: %q", tool.Description)
			}

			if !strings.Contains(tool.Description, "trigger operation") || !strings.Contains(tool.Description, "does not guarantee workload completion") {
				t.Fatalf("trigger_scheduled_job description overstates completion semantics: %q", tool.Description)
			}
		}

		if tool.Name == "trigger_poll" {
			assertMCPProjectToolAnnotations(t, tool, true, false)

			if !strings.Contains(tool.Description, "Prefer wait=false and poll get_deployment_run") || !strings.Contains(tool.Description, "10s grace") {
				t.Fatalf("trigger_poll description lacks wait guidance: %q", tool.Description)
			}

			configsSchema := toolSchemaProperty(t, tool.InputSchema, "configs")
			if configsSchema["maxItems"] != float64(controlplane.MaxTriggerPollConfigs) {
				t.Fatalf("trigger_poll configs maxItems = %#v, want %d", configsSchema["maxItems"], controlplane.MaxTriggerPollConfigs)
			}

			items, ok := configsSchema["items"].(map[string]any)
			if !ok {
				t.Fatalf("trigger_poll configs items schema = %#v", configsSchema["items"])
			}

			items = resolveToolSchemaRef(t, tool.InputSchema, items)

			for _, property := range []string{"source", "url", "reference", "target", "deployments"} {
				if !toolSchemaHasProperty(items, property) {
					t.Fatalf("trigger_poll config schema lacks %q: %#v", property, items)
				}
			}

			if !toolSchemaRequiresProperty(items, "url") {
				t.Fatalf("trigger_poll config schema must require url: %#v", items)
			}

			if toolSchemaRequiresProperty(items, "reference") {
				t.Fatalf("trigger_poll config schema must not require reference: %#v", items)
			}

			for _, property := range []string{"source", "target", "deployments"} {
				if toolSchemaRequiresProperty(items, property) {
					t.Fatalf("trigger_poll config schema must not require %s: %#v", property, items)
				}
			}

			for _, property := range []string{"interval", "run_once"} {
				if toolSchemaHasProperty(items, property) {
					t.Fatalf("trigger_poll config schema exposes ignored property %q: %#v", property, items)
				}
			}

			deploymentsSchema := toolSchemaProperty(t, items, "deployments")

			deploymentItems, ok := deploymentsSchema["items"].(map[string]any)
			if !ok {
				t.Fatalf("trigger_poll deployments items schema = %#v", deploymentsSchema["items"])
			}

			deploymentItems = resolveToolSchemaRef(t, tool.InputSchema, deploymentItems)
			if toolSchemaHasProperty(deploymentItems, "Internal") || toolSchemaHasProperty(deploymentItems, "internal") {
				t.Fatalf("trigger_poll deployment schema exposes internal state: %#v", deploymentItems)
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

func TestMCPServerGetHealth(t *testing.T) {
	server, _ := newMCPTestServer(t, true, testMCPAPIKey, 1024)
	session := connectMCPTestClient(t, server)
	requestsBefore := testutil.ToFloat64(prometheusmetrics.McpRequestsTotal.WithLabelValues("get_health"))
	errorsBefore := testutil.ToFloat64(prometheusmetrics.McpErrorsTotal.WithLabelValues("get_health"))
	durationsBefore := histogramSampleCount(t, prometheusmetrics.McpRequestDuration.WithLabelValues("get_health"))

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_health", Arguments: struct{}{}})
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

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_health", Arguments: struct{}{}})
	if err != nil {
		t.Fatal(err)
	}

	if !result.IsError {
		t.Fatalf("expected get_health tool error, got %#v", result)
	}

	encodedContent, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatal(err)
	}

	// The error must carry the connection-kind sentinel so operators can tell
	// a DOCKER_HOST failure from a socket failure.
	if !strings.Contains(string(encodedContent), docker.ErrDockerHostConnectionFailed.Error()) {
		t.Fatalf("get_health error lacks connection sentinel: %s", encodedContent)
	}

	errorsAfter := testutil.ToFloat64(prometheusmetrics.McpErrorsTotal.WithLabelValues("get_health"))
	if errorsAfter-errorsBefore != 1 {
		t.Fatalf("expected one error metric increment, got delta %v", errorsAfter-errorsBefore)
	}
}

func TestMCPServerRejectsOversizedBody(t *testing.T) {
	server, _ := newMCPTestServer(t, true, testMCPAPIKey, 32)

	request, err := http.NewRequest(http.MethodPost, server.URL+testMCPPath, bytes.NewReader(make([]byte, 33)))
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
	handler := instrumentTool(logger.New(logger.LevelCritical), toolName, func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, struct{}, error) {
		return &sdkmcp.CallToolResult{IsError: true}, struct{}{}, nil
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
	handler := instrumentTool(logger.New(logger.LevelCritical), toolName, func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, struct{}, error) {
		return &sdkmcp.CallToolResult{IsError: true}, struct{}{}, errors.New("failed")
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
		result     *sdkmcp.CallToolResult
		err        error
		wantDetail string
	}{
		{name: "handler error", result: &sdkmcp.CallToolResult{IsError: true}, err: errors.New("handler failed"), wantDetail: "handler failed"},
		{name: "error result", result: &sdkmcp.CallToolResult{IsError: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var logOutput bytes.Buffer

			log := &logger.Logger{
				Logger: slog.New(slog.NewTextHandler(&logOutput, nil)),
				Level:  slog.LevelDebug,
			}
			handler := instrumentTool(log, "test_failure", func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, struct{}, error) {
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

func resolveToolSchemaRef(t *testing.T, root any, schema map[string]any) map[string]any {
	t.Helper()

	ref, ok := schema["$ref"].(string)
	if !ok {
		return schema
	}

	const defsPrefix = "#/$defs/"
	if !strings.HasPrefix(ref, defsPrefix) {
		t.Fatalf("unsupported schema reference %q", ref)
	}

	rootMap, ok := root.(map[string]any)
	if !ok {
		t.Fatalf("expected root object schema, got %T", root)
	}

	defs, ok := rootMap["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("schema reference %q has no definitions: %#v", ref, rootMap)
	}

	resolved, ok := defs[strings.TrimPrefix(ref, defsPrefix)].(map[string]any)
	if !ok {
		t.Fatalf("schema reference %q is unresolved: %#v", ref, defs)
	}

	return resolved
}

func assertMCPProjectToolAnnotations(t *testing.T, tool *sdkmcp.Tool, destructive, idempotent bool) {
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

func assertMCPProjectTimeoutSchema(t *testing.T, tool *sdkmcp.Tool) {
	t.Helper()

	timeoutSchema := toolSchemaProperty(t, tool.InputSchema, "timeout")
	if timeoutSchema["minimum"] != float64(1) {
		t.Fatalf("%s timeout minimum = %#v, want 1", tool.Name, timeoutSchema["minimum"])
	}

	if timeoutSchema["maximum"] != float64(controlplane.MaxProjectActionTimeout) {
		t.Fatalf("%s timeout maximum = %#v, want %d", tool.Name, timeoutSchema["maximum"], controlplane.MaxProjectActionTimeout)
	}
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
