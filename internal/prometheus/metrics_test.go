package prometheus

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	clientPrometheus "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kimdre/doco-cd/internal/config/app"
)

// TestServe tests the metrics endpoint serving functionality.
func TestServe(t *testing.T) {
	t.Parallel()

	expectedStatusCode := 200
	expectedContentType := "text/plain; version=0.0.4; charset=utf-8; escaping=underscores"

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	AppInfo.WithLabelValues("test", appConfig.LogLevel, time.Now().Format(time.RFC3339)).Set(1)
	ScheduledRunsTotal.WithLabelValues("test-stack", "backup", "container", "restart").Inc()

	req, err := http.NewRequest("GET", MetricsPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()

	handler := promhttp.Handler()
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != expectedStatusCode {
		t.Errorf("Expected status code %d, got %d", expectedStatusCode, status)
	}

	if contentType := rr.Header().Get("Content-Type"); contentType != expectedContentType {
		t.Errorf("Expected Content-Type %s, got %s", expectedContentType, contentType)
	}

	// Check if the response body is not empty
	if rr.Body.Len() == 0 {
		t.Error("Expected non-empty response body, got empty")
	}

	// Check if the response body contains the expected metrics
	if !strings.Contains(rr.Body.String(), "doco_cd_info") {
		t.Error("Expected response body to contain 'doco_cd_info' metric, but it does not")
	}

	if !strings.Contains(rr.Body.String(), "doco_cd_scheduled_runs_total") {
		t.Error("Expected response body to contain 'doco_cd_scheduled_runs_total' metric, but it does not")
	}
}

func TestDeploymentMetricsIncludeRepositoryAndDeploymentLabels(t *testing.T) {
	t.Parallel()

	DeploymentsTotal.WithLabelValues("github.com/example/repo", "test-stack").Inc()

	req, err := http.NewRequest("GET", MetricsPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	promhttp.Handler().ServeHTTP(rr, req)

	linePattern := regexp.MustCompile(`doco_cd_deployments_total\{[^}]*deployment="test-stack"[^}]*repository="github.com/example/repo"[^}]*\}\s+1`)
	if !linePattern.MatchString(rr.Body.String()) {
		t.Fatalf("expected deployments_total with repository and deployment labels, got:\n%s", rr.Body.String())
	}
}

func TestMCPMetricsAreRegistered(t *testing.T) {
	t.Parallel()

	McpRequestsTotal.WithLabelValues("list_projects").Inc()
	McpErrorsTotal.WithLabelValues("list_projects").Inc()
	McpRequestDuration.WithLabelValues("list_projects").Observe(0.1)

	metricFamilies, err := clientPrometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	metricNames := make([]string, 0, len(metricFamilies))
	for _, metricFamily := range metricFamilies {
		metricNames = append(metricNames, metricFamily.GetName())
	}

	for _, expectedName := range []string{
		"doco_cd_mcp_requests_total",
		"doco_cd_mcp_errors_total",
		"doco_cd_mcp_request_duration_seconds",
	} {
		if !slices.Contains(metricNames, expectedName) {
			t.Errorf("expected gathered metrics to contain %q", expectedName)
		}
	}

	expectedHelp := map[string]string{
		"doco_cd_mcp_requests_total":           "Total number of dispatched MCP tool calls",
		"doco_cd_mcp_errors_total":             "Total number of failed dispatched MCP tool calls",
		"doco_cd_mcp_request_duration_seconds": "Duration of dispatched MCP tool calls in seconds",
	}
	for _, metricFamily := range metricFamilies {
		if help, ok := expectedHelp[metricFamily.GetName()]; ok && metricFamily.GetHelp() != help {
			t.Errorf("expected %s help %q, got %q", metricFamily.GetName(), help, metricFamily.GetHelp())
		}
	}
}
