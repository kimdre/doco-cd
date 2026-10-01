package docker

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/deploy"
)

func TestNormalizeDeploymentPhase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "phase provided",
			input: "pulling images",
			want:  "pulling images",
		},
		{
			name:  "phase empty",
			input: "",
			want:  "unknown",
		},
		{
			name:  "phase whitespace",
			input: "   ",
			want:  "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := normalizeDeploymentPhase(tt.input); got != tt.want {
				t.Fatalf("normalizeDeploymentPhase() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLogDeploymentHeartbeat_EmitsPhaseField(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	logDeploymentHeartbeat(logger, "pulling images")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to decode log json: %v", err)
	}

	msg, _ := entry["msg"].(string)
	if msg != "deployment in progress" {
		t.Fatalf("unexpected message: %q", msg)
	}

	phase, _ := entry["phase"].(string)
	if phase != "pulling images" {
		t.Fatalf("unexpected phase field: %q", phase)
	}
}

type recordingPhaseReporter struct {
	mu      sync.Mutex
	phases  []string
	stopped int
}

func (r *recordingPhaseReporter) Report(phase string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.phases = append(r.phases, phase)
}

func (r *recordingPhaseReporter) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.stopped++
}

func TestDeploymentPhaseState_ReportsOnlyChanges(t *testing.T) {
	t.Parallel()

	reporter := &recordingPhaseReporter{}
	state := newDeploymentPhaseState("resolving working directory", reporter)

	state.Set("pulling images")
	state.Set(" pulling images ")
	state.Set("creating services")
	state.Set("")

	want := []string{"resolving working directory", "pulling images", "creating services", "unknown"}
	if !slices.Equal(reporter.phases, want) {
		t.Fatalf("reported phases = %q, want %q", reporter.phases, want)
	}

	if got := state.Get(); got != "unknown" {
		t.Fatalf("Get() = %q, want unknown", got)
	}
}

func TestDeploymentPhaseState_WithoutReporter(t *testing.T) {
	t.Parallel()

	state := newDeploymentPhaseState("pulling images", nil)
	state.Set("creating services")
	state.stopReporting()

	if got := state.Get(); got != "creating services" {
		t.Fatalf("Get() = %q, want creating services", got)
	}
}

func TestSelfDeployInput_StopsPhaseReports(t *testing.T) {
	t.Parallel()

	reporter := &recordingPhaseReporter{}
	req := runtimeDeployRequest{
		request: DeployRequest{DeployConfig: &deploy.Config{}},
		phase:   newDeploymentPhaseState("deploying compose stack", reporter),
	}

	req.selfDeployInput().stopPhaseReports()

	if reporter.stopped != 1 {
		t.Fatalf("reporter stopped %d times, want 1", reporter.stopped)
	}

	var nilInput *SelfDeployInput

	nilInput.stopPhaseReports()
	(&SelfDeployInput{}).stopPhaseReports()
}
