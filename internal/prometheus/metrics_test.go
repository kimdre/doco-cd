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

	"github.com/kimdre/doco-cd/internal/config/app"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
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
	ScheduledRunsTotal.WithLabelValues("default", "test-stack", "backup", "container", "restart").Inc()

	req, err := http.NewRequest("GET", MetricsPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()

	handler := Handler()
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

func TestDeploymentMetricsIncludeContextLabel(t *testing.T) {
	t.Parallel()

	labels := []string{"github.com/example/repo", "test-stack", "remote"}

	// The metric is process-global; start from a fresh series so repeated runs (-count) see 1.
	DeploymentsTotal.DeleteLabelValues(labels...)
	t.Cleanup(func() { DeploymentsTotal.DeleteLabelValues(labels...) })

	DeploymentsTotal.WithLabelValues(labels...).Inc()

	req, err := http.NewRequest("GET", MetricsPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	Handler().ServeHTTP(rr, req)

	linePattern := regexp.MustCompile(`doco_cd_deployments_total\{[^}]*context="remote"[^}]*deployment="test-stack"[^}]*repository="github.com/example/repo"[^}]*\}\s+1`)
	if !linePattern.MatchString(rr.Body.String()) {
		t.Fatalf("expected deployments_total with context label, got:\n%s", rr.Body.String())
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

	// Metric names are the operational contract (dashboards and alerts key on
	// them); help text is documentation and intentionally not pinned here.
	for _, expectedName := range []string{
		"doco_cd_mcp_requests_total",
		"doco_cd_mcp_errors_total",
		"doco_cd_mcp_request_duration_seconds",
	} {
		if !slices.Contains(metricNames, expectedName) {
			t.Errorf("expected gathered metrics to contain %q", expectedName)
		}
	}
}

func TestLongRunningHistogramsUseDurationBuckets(t *testing.T) {
	t.Parallel()

	if got := DurationBuckets[len(DurationBuckets)-1]; got < 300 {
		t.Fatalf("largest duration bucket = %v, want at least 300s", got)
	}

	for _, bucket := range clientPrometheus.DefBuckets {
		if !slices.Contains(DurationBuckets, bucket) {
			t.Errorf("DurationBuckets is missing default bucket %v", bucket)
		}
	}

	SourcePreparationDuration.WithLabelValues("git", "success").Observe(42)

	metricFamilies, err := clientPrometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	for _, metricFamily := range metricFamilies {
		if metricFamily.GetName() != "doco_cd_source_preparation_duration_seconds" {
			continue
		}

		buckets := metricFamily.GetMetric()[0].GetHistogram().GetBucket()
		if got := buckets[len(buckets)-1].GetUpperBound(); got != DurationBuckets[len(DurationBuckets)-1] {
			t.Fatalf("source preparation largest bucket = %v, want %v", got, DurationBuckets[len(DurationBuckets)-1])
		}

		return
	}

	t.Fatal("source preparation histogram not gathered")
}

func TestGitMirrorLockMetricsAreObserved(t *testing.T) {
	t.Parallel()

	// The observer is installed by init; a mirror lock round trip feeds it.
	gitInternal.AcquireSharedMirrorLock(t.TempDir())()

	metricFamilies, err := clientPrometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	observed := map[string]bool{}

	for _, metricFamily := range metricFamilies {
		for _, metric := range metricFamily.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "mode" && label.GetValue() == gitInternal.MirrorLockShared && metric.GetHistogram().GetSampleCount() > 0 {
					observed[metricFamily.GetName()] = true
				}
			}
		}
	}

	for _, name := range []string{"doco_cd_git_mirror_lock_wait_seconds", "doco_cd_git_mirror_lock_held_seconds"} {
		if !observed[name] {
			t.Errorf("expected %s to have a shared-mode observation", name)
		}
	}
}
