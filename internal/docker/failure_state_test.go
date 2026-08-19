package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeploymentFailureRoundtrip(t *testing.T) {
	dataDir := t.TempDir()
	repo := "github.com/example/app"
	stack := "app"

	if _, ok := GetDeploymentFailure(dataDir, repo, stack); ok {
		t.Fatal("expected no failure marker before recording")
	}

	want := DeploymentFailure{
		Repository: repo,
		Stack:      stack,
		CommitSHA:  "573a16e",
		Stage:      "deploy",
		Error:      "hook exited with status 1",
		FailedAt:   time.Now().UTC().Truncate(time.Second),
	}

	if err := RecordDeploymentFailure(dataDir, repo, stack, want); err != nil {
		t.Fatalf("failed to record failure: %v", err)
	}

	got, ok := GetDeploymentFailure(dataDir, repo, stack)
	if !ok {
		t.Fatal("expected failure marker after recording")
	}

	if got != want {
		t.Fatalf("marker mismatch: got %+v, want %+v", got, want)
	}

	if err := ClearDeploymentFailure(dataDir, repo, stack); err != nil {
		t.Fatalf("failed to clear failure marker: %v", err)
	}

	if _, ok := GetDeploymentFailure(dataDir, repo, stack); ok {
		t.Fatal("expected no failure marker after clearing")
	}
}

func TestClearDeploymentFailureMissingMarker(t *testing.T) {
	if err := ClearDeploymentFailure(t.TempDir(), "github.com/example/app", "app"); err != nil {
		t.Fatalf("expected no error for a missing marker, got %v", err)
	}
}

func TestGetDeploymentFailureCorruptedMarkerStillCounts(t *testing.T) {
	dataDir := t.TempDir()
	repo := "github.com/example/app"
	stack := "app"

	path := deploymentFailurePath(dataDir, repo, stack)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("failed to create marker dir: %v", err)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("failed to write corrupted marker: %v", err)
	}

	if _, ok := GetDeploymentFailure(dataDir, repo, stack); !ok {
		t.Fatal("corrupted marker must still count as a failure")
	}
}

func TestRecordDeploymentFailureTruncatesError(t *testing.T) {
	dataDir := t.TempDir()
	repo := "github.com/example/app"
	stack := "app"

	failure := DeploymentFailure{Error: strings.Repeat("x", maxRecordedErrorLength+100)}
	if err := RecordDeploymentFailure(dataDir, repo, stack, failure); err != nil {
		t.Fatalf("failed to record failure: %v", err)
	}

	got, ok := GetDeploymentFailure(dataDir, repo, stack)
	if !ok {
		t.Fatal("expected failure marker after recording")
	}

	if len(got.Error) != maxRecordedErrorLength {
		t.Fatalf("expected error truncated to %d chars, got %d", maxRecordedErrorLength, len(got.Error))
	}
}

func TestSanitizeStateFileName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"github.com/user/repo", "github.com_user_repo"},
		{"stack-name_1.0", "stack-name_1.0"},
		{"", "_"},
		{"a b:c", "a_b_c"},
	}

	for _, tt := range tests {
		if got := sanitizeStateFileName(tt.in); got != tt.want {
			t.Errorf("sanitizeStateFileName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestForcedRecreateServices(t *testing.T) {
	tests := []struct {
		name    string
		changes []Change
		want    []string // nil means force the whole project
	}{
		{
			name:    "scoped changes collect services",
			changes: []Change{{Type: "configs", Services: []string{"web"}}, {Type: "env_files", Services: []string{"db"}}},
			want:    []string{"db", "web"},
		},
		{
			name:    "unscoped change forces whole project",
			changes: []Change{{Type: "configs", Services: []string{"web"}}, {Type: ChangeTypeFailedDeployRetry}},
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := forcedRecreateServices(tt.changes).ToSlice()
			if len(got) != len(tt.want) {
				t.Fatalf("forcedRecreateServices() = %v, want %v", got, tt.want)
			}

			for _, svc := range tt.want {
				if !forcedRecreateServices(tt.changes).Contains(svc) {
					t.Fatalf("expected %q in forced services %v", svc, got)
				}
			}
		})
	}
}
