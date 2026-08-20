package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/logger"
	prometheusmetrics "github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/restapi"
)

const testMCPAPIKey = "test-mcp-api-key" // #nosec G101 -- test fixture, not a real credential.

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

func TestMCPServerListsGetHealth(t *testing.T) {
	server, _ := newMCPTestServer(t, true, testMCPAPIKey, 1024)
	session := connectMCPTestClient(t, server)

	result, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Tools) != 1 {
		t.Fatalf("expected exactly one MCP tool, got %#v", result.Tools)
	}

	tool := result.Tools[0]
	if tool.Name != "get_health" {
		t.Fatalf("expected get_health, got %q", tool.Name)
	}

	if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		t.Fatalf("get_health must have readOnlyHint=true: %#v", tool.Annotations)
	}
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
	handler := instrumentMCPTool(toolName, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
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
	handler := instrumentMCPTool(toolName, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
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

func newMCPTestServer(t *testing.T, enabled bool, apiSecret string, maxPayloadSize int64) (*httptest.Server, []string) {
	t.Helper()

	config := &app.Config{
		ApiSecret:      apiSecret,
		McpEnabled:     enabled,
		MaxPayloadSize: maxPayloadSize,
	}
	log := logger.New(logger.LevelCritical)
	handler := &handlerData{appConfig: &app.Config{}, appVersion: app.Version, log: log}
	mux := http.NewServeMux()
	enabledEndpoints := registerApiEndpoints(config, handler, log, mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server, enabledEndpoints
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
	for _, endpoint := range endpoints {
		if endpoint == target {
			return true
		}
	}

	return false
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
