package stages

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/deploy"
)

func newDestroyStageManager(t *testing.T, removeDir bool, removals *RepositoryRemovals) (*StageManager, string) {
	t.Helper()

	dataDir := t.TempDir()
	repoName := "github.com/owner/repo"
	repoDir := filepath.Join(dataDir, filepath.FromSlash(repoName))

	if err := os.MkdirAll(filepath.Join(repoDir, "artifacts", "abc"), 0o755); err != nil {
		t.Fatalf("create artifact dir: %v", err)
	}

	dc := &deploy.Config{Name: "stack"}
	dc.Destroy.Enabled = true
	dc.Destroy.RemoveRepoDir = removeDir

	return &StageManager{
		Log:          slog.New(slog.DiscardHandler),
		DeployConfig: dc,
		Docker: &Docker{
			DataMountPoint: container.MountPoint{Source: dataDir, Destination: dataDir},
		},
		Repository:         &RepositoryData{Name: repoName},
		RepositoryRemovals: removals,
	}, repoDir
}

// TestRequestRepositoryRemoval_RecordsWithoutRemoving checks that a destroy with
// remove_dir only records the request. Other stacks can still mount files from the
// repository directory, so the destroy stage must not remove it (#1962).
func TestRequestRepositoryRemoval_RecordsWithoutRemoving(t *testing.T) {
	t.Parallel()

	removals := NewRepositoryRemovals()
	s, repoDir := newDestroyStageManager(t, true, removals)

	s.requestRepositoryRemoval(s.Log)

	if got, want := removals.Names(), []string{"github.com/owner/repo"}; !slices.Equal(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}

	if _, err := os.Stat(filepath.Join(repoDir, "artifacts", "abc")); err != nil {
		t.Fatalf("destroy stage touched the repository directory: %v", err)
	}
}

func TestRequestRepositoryRemoval_RemoveDirDisabled(t *testing.T) {
	t.Parallel()

	removals := NewRepositoryRemovals()
	s, repoDir := newDestroyStageManager(t, false, removals)

	s.requestRepositoryRemoval(s.Log)

	if got := removals.Names(); len(got) != 0 {
		t.Fatalf("Names() = %v, want none", got)
	}

	if _, err := os.Stat(repoDir); err != nil {
		t.Fatalf("repository directory was removed: %v", err)
	}
}

func TestRequestRepositoryRemoval_WithoutRemovalsKeepsDirectory(t *testing.T) {
	t.Parallel()

	s, repoDir := newDestroyStageManager(t, true, nil)

	s.requestRepositoryRemoval(s.Log)

	if _, err := os.Stat(repoDir); err != nil {
		t.Fatalf("repository directory was removed: %v", err)
	}
}

func TestRepositoryRemovals_ConcurrentAddIsDeduplicated(t *testing.T) {
	t.Parallel()

	removals := NewRepositoryRemovals()

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			if i%2 == 0 {
				removals.Add("b/repo")
			} else {
				removals.Add("a/repo")
			}

			removals.Add("")
		})
	}

	wg.Wait()

	if got, want := removals.Names(), []string{"a/repo", "b/repo"}; !slices.Equal(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}

	var nilRemovals *RepositoryRemovals

	nilRemovals.Add("ignored")

	if got := nilRemovals.Names(); got != nil {
		t.Fatalf("nil Names() = %v, want nil", got)
	}
}
