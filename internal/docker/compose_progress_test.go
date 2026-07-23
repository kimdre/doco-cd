package docker

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
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
