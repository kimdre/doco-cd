package docker

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDeploymentFailureRoundtrip(t *testing.T) {
	t.Parallel()

	repo, stack := "github.com/example/app", "roundtrip"

	t.Cleanup(func() { ClearDeploymentFailure(repo, stack) })

	if _, ok := GetDeploymentFailure(repo, stack); ok {
		t.Fatal("expected no failure record before recording")
	}

	want := DeploymentFailure{
		Repository: repo,
		Stack:      stack,
		CommitSHA:  "573a16e",
		Stage:      "deploy",
		Error:      "service web is unhealthy",
		Services:   []string{"web"},
		FailedAt:   time.Now().UTC().Truncate(time.Second),
	}

	RecordDeploymentFailure(repo, stack, want)

	got, ok := GetDeploymentFailure(repo, stack)
	if !ok {
		t.Fatal("expected failure record after recording")
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record mismatch: got %+v, want %+v", got, want)
	}

	// The record must not share its services with the failures handed in or out.
	want.Services[0] = "changed-by-caller"
	got.Services[0] = "changed-by-reader"

	if again, _ := GetDeploymentFailure(repo, stack); !slices.Equal(again.Services, []string{"web"}) {
		t.Fatalf("recorded services = %v, want [web]", again.Services)
	}

	ClearDeploymentFailure(repo, stack)

	if _, ok := GetDeploymentFailure(repo, stack); ok {
		t.Fatal("expected no failure record after clearing")
	}
}

func TestClearDeploymentFailureMissingRecord(t *testing.T) {
	t.Parallel()

	// must be a no-op, not a panic
	ClearDeploymentFailure("github.com/example/app", "never-recorded")
}

func TestRecordDeploymentFailureTruncatesError(t *testing.T) {
	t.Parallel()

	repo, stack := "github.com/example/app", "truncate"

	t.Cleanup(func() { ClearDeploymentFailure(repo, stack) })

	RecordDeploymentFailure(repo, stack, DeploymentFailure{Error: strings.Repeat("x", maxRecordedErrorLength+100)})

	got, ok := GetDeploymentFailure(repo, stack)
	if !ok {
		t.Fatal("expected failure record after recording")
	}

	if len(got.Error) != maxRecordedErrorLength {
		t.Fatalf("expected error truncated to %d chars, got %d", maxRecordedErrorLength, len(got.Error))
	}
}

func TestForcedRecreateServices(t *testing.T) {
	t.Parallel()

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
		{
			name:    "scoped retry only forces the failed services",
			changes: []Change{{Type: "configs", Services: []string{"web"}}, {Type: ChangeTypeFailedDeployRetry, Services: []string{"db"}}},
			want:    []string{"db", "web"},
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

func TestStaleArtifactServices(t *testing.T) {
	t.Parallel()

	changes := []Change{
		{Type: "modified", Services: []string{"web"}},
		{Type: ChangeTypeStaleArtifact, Services: []string{"db", "cache"}},
		{Type: ChangeTypeFailedDeployRetry},
		{Type: ChangeTypeStaleArtifact, Services: []string{"db"}},
	}

	got := staleArtifactServices(changes)
	slices.Sort(got)

	if want := []string{"cache", "db"}; !slices.Equal(got, want) {
		t.Fatalf("staleArtifactServices() = %v, want %v", got, want)
	}
}
