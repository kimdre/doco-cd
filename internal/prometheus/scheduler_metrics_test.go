package prometheus

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// TestScheduledRunMetrics_ContextLabel verifies that every scheduler metric
// carries a "context" label (so runs on different Docker contexts can be
// distinguished), and that the skipped-run metric still exposes "reason" as
// its last label, per convention.
func TestScheduledRunMetrics_ContextLabel(t *testing.T) {
	t.Parallel()

	ScheduledRunsTotal.WithLabelValues("remote", "test-context-stack", "backup", "container", "one_off").Inc()
	ScheduledRunErrorsTotal.WithLabelValues("remote", "test-context-stack", "backup", "container", "one_off").Inc()
	ScheduledRunSkippedTotal.WithLabelValues("remote", "test-context-stack", "backup", "container", "one_off", "still_running").Inc()
	ScheduledRunDuration.WithLabelValues("remote", "test-context-stack", "backup", "container", "one_off").Observe(1)
	ScheduledRunsActive.WithLabelValues("remote", "test-context-stack", "backup", "container", "one_off").Inc()

	req, err := http.NewRequest(http.MethodGet, MetricsPath, nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	rr := httptest.NewRecorder()
	promhttp.Handler().ServeHTTP(rr, req)

	body, err := io.ReadAll(rr.Result().Body)
	if err != nil {
		t.Fatalf("failed to read metrics response body: %v", err)
	}

	out := string(body)

	for _, want := range []string{
		`doco_cd_scheduled_runs_total{context="remote",execution_mode="one_off",job="backup",mode="container",stack="test-context-stack"} 1`,
		`doco_cd_scheduled_run_errors_total{context="remote",execution_mode="one_off",job="backup",mode="container",stack="test-context-stack"} 1`,
		`doco_cd_scheduled_run_skipped_total{context="remote",execution_mode="one_off",job="backup",mode="container",reason="still_running",stack="test-context-stack"} 1`,
		`doco_cd_scheduled_runs_active{context="remote",execution_mode="one_off",job="backup",mode="container",stack="test-context-stack"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected metrics output to contain %q, got:\n%s", want, out)
		}
	}
}
