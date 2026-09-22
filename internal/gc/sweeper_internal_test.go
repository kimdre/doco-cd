package gc

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/source"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// makeArtifactDir creates "<repoDir>/artifacts/<revision>" directly on disk,
// bypassing the store package's own publish path since these tests only
// need a directory tree store.Sweep can walk.
func makeArtifactDir(t *testing.T, repoDir string, revision store.Revision) string {
	t.Helper()

	path := filepath.Join(repoDir, store.ArtifactsSubdir, string(revision))
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("create artifact dir %s: %v", path, err)
	}

	return path
}

func newTestSweeper(t *testing.T) (*Sweeper, string) {
	t.Helper()

	dataMountPoint := t.TempDir()

	s := &Sweeper{
		log:            testLogger(),
		dataMountPoint: dataMountPoint,
		opts:           store.GCOptions{RetentionRecords: 0, RetentionTTL: time.Minute},
		interval:       time.Hour,
		now:            time.Now,
		listRepos:      listRepositoryDirs,
		sweepDirectory: store.Sweep,
		liveRevisions: func(context.Context, *docker.ContextRegistry, *slog.Logger, string, string) (map[string]set.Set[store.Revision], error) {
			return nil, nil
		},
	}

	return s, dataMountPoint
}

func TestSweeper_SweepRepoDir_RemovesUnreferencedExpiredArtifact(t *testing.T) {
	t.Parallel()

	s, dataMountPoint := newTestSweeper(t)

	repoDir := filepath.Join(dataMountPoint, "github.com", "owner", "example-repo")
	artifactPath := makeArtifactDir(t, repoDir, "expired")

	// Backdate well past the 1-minute TTL configured in newTestSweeper.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(artifactPath, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	s.sweepRepoDir(repoDir, nil)

	if _, err := os.Stat(artifactPath); !os.IsNotExist(err) {
		t.Fatalf("expired artifact still exists, stat err = %v", err)
	}
}

func TestSweeper_SweepRepoDir_KeepsLiveArtifact(t *testing.T) {
	t.Parallel()

	s, dataMountPoint := newTestSweeper(t)

	repoDir := filepath.Join(dataMountPoint, "github.com", "owner", "example-repo")
	artifactPath := makeArtifactDir(t, repoDir, "live")

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(artifactPath, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Keyed the way LiveRevisions keys it: the normalized source-name label,
	// which carries the provider full name without the host segment the
	// on-disk path has.
	live := map[string]set.Set[store.Revision]{
		"owner/example-repo": set.New[store.Revision]("live"),
	}

	s.sweepRepoDir(repoDir, live)

	if _, err := os.Stat(artifactPath); err != nil {
		t.Fatalf("live artifact was removed unexpectedly, stat err = %v", err)
	}
}

func TestSweeper_SweepRepoDir_KeepsInFlightArtifactEvenWithoutLabel(t *testing.T) {
	t.Parallel()

	s, dataMountPoint := newTestSweeper(t)

	repoDir := filepath.Join(dataMountPoint, "github.com", "owner", "example-repo")
	artifactPath := makeArtifactDir(t, repoDir, "inflight")

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(artifactPath, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// The in-flight key is the repository path relative to the data mount
	// point, exactly what Prepare marks - not the directory's base name.
	release := source.MarkInFlight("github.com/owner/example-repo", "inflight")
	defer release()

	s.sweepRepoDir(repoDir, nil)

	if _, err := os.Stat(artifactPath); err != nil {
		t.Fatalf("in-flight artifact was removed unexpectedly, stat err = %v", err)
	}
}

func TestSweeper_Sweep_ProcessesEveryRepoDirUnderDataMountPoint(t *testing.T) {
	t.Parallel()

	s, dataMountPoint := newTestSweeper(t)

	var repos []string

	for _, name := range []string{"repo-a", "repo-b"} {
		repoDir := filepath.Join(dataMountPoint, "github.com", "owner", name)
		artifactPath := makeArtifactDir(t, repoDir, "expired")

		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(artifactPath, old, old); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}

		repos = append(repos, repoDir)
	}

	s.sweep(context.Background())

	for _, repoDir := range repos {
		if _, err := os.Stat(filepath.Join(repoDir, store.ArtifactsSubdir, "expired")); !os.IsNotExist(err) {
			t.Errorf("expired artifact under %s still exists, stat err = %v", repoDir, err)
		}
	}
}

func TestSweeper_Sweep_StopsOnContextCancellation(t *testing.T) {
	t.Parallel()

	s, dataMountPoint := newTestSweeper(t)

	repoDir := filepath.Join(dataMountPoint, "github.com", "owner", "repo-a")
	artifactPath := makeArtifactDir(t, repoDir, "expired")

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(artifactPath, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.sweep(ctx)

	// A cancelled context must stop the sweep before it processes any
	// repository directory, so nothing should have been removed.
	if _, err := os.Stat(artifactPath); err != nil {
		t.Fatalf("artifact should not have been touched after context cancellation, stat err = %v", err)
	}
}

func TestSweeper_Sweep_SkipsAllRepositoriesWhenLiveDiscoveryFails(t *testing.T) {
	t.Parallel()

	s, dataMountPoint := newTestSweeper(t)

	repoDir := filepath.Join(dataMountPoint, "github.com", "owner", "repo-a")
	artifactPath := makeArtifactDir(t, repoDir, "expired")

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(artifactPath, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	s.liveRevisions = func(context.Context, *docker.ContextRegistry, *slog.Logger, string, string) (map[string]set.Set[store.Revision], error) {
		return nil, errors.New("context unavailable")
	}

	s.sweep(context.Background())

	if _, err := os.Stat(artifactPath); err != nil {
		t.Fatalf("artifact should not be removed after incomplete live discovery, stat err = %v", err)
	}
}

func TestSweeper_Start_NoopWhenUnconfigured(t *testing.T) {
	t.Parallel()

	// Must return immediately without panicking when required fields are
	// missing, mirroring internal/certrotation.Watcher's own guard.
	(&Sweeper{}).Start(context.Background())
}

// TestListRepositoryDirs_FindsNestedRepoRoots guards the layout that actually
// exists on disk: git.GetRepoName produces "<host>/<owner>/<repo>", so a
// repository root is never an immediate child of the data mount point. Listing
// only immediate children turned the whole sweeper into a no-op.
func TestListRepositoryDirs_FindsNestedRepoRoots(t *testing.T) {
	t.Parallel()

	dataMountPoint := t.TempDir()

	gitRepo := filepath.Join(dataMountPoint, "github.com", "owner", "repo-a")
	makeArtifactDir(t, gitRepo, "rev")

	if err := os.MkdirAll(filepath.Join(gitRepo, "mirror"), 0o755); err != nil {
		t.Fatalf("mkdir mirror: %v", err)
	}

	// Deeper nesting, e.g. a GitLab subgroup.
	nested := filepath.Join(dataMountPoint, "gitlab.com", "group", "subgroup", "repo-b")
	makeArtifactDir(t, nested, "rev")

	// Not a store base directory, and must not be reported.
	if err := os.MkdirAll(filepath.Join(dataMountPoint, "unrelated", "stuff"), 0o755); err != nil {
		t.Fatalf("mkdir unrelated: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dataMountPoint, "stray-file.txt"), []byte(""), 0o600); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	dirs, err := listRepositoryDirs(dataMountPoint)
	if err != nil {
		t.Fatalf("listRepositoryDirs() error = %v", err)
	}

	got := set.New(dirs...)

	want := []string{gitRepo, nested}
	if got.Len() != len(want) {
		t.Fatalf("listRepositoryDirs() = %v, want exactly %v", dirs, want)
	}

	for _, dir := range want {
		if !got.Contains(dir) {
			t.Errorf("listRepositoryDirs() = %v, missing %q", dirs, dir)
		}
	}

	dirs, err = listRepositoryDirs(filepath.Join(dataMountPoint, "does-not-exist"))
	if err != nil {
		t.Fatalf("listRepositoryDirs(missing) error = %v, want nil", err)
	}

	if len(dirs) != 0 {
		t.Fatalf("listRepositoryDirs(missing) = %v, want empty", dirs)
	}
}

// TestListRepositoryDirs_DoesNotDescendIntoArtifacts makes sure a published
// artifact that happens to contain an "artifacts" or "mirror" directory of its
// own is never mistaken for a repository root.
func TestListRepositoryDirs_DoesNotDescendIntoArtifacts(t *testing.T) {
	t.Parallel()

	dataMountPoint := t.TempDir()

	repoDir := filepath.Join(dataMountPoint, "github.com", "owner", "repo")
	artifact := makeArtifactDir(t, repoDir, "rev")

	if err := os.MkdirAll(filepath.Join(artifact, "mirror"), 0o755); err != nil {
		t.Fatalf("mkdir nested mirror: %v", err)
	}

	dirs, err := listRepositoryDirs(dataMountPoint)
	if err != nil {
		t.Fatalf("listRepositoryDirs() error = %v", err)
	}

	if len(dirs) != 1 || dirs[0] != repoDir {
		t.Fatalf("listRepositoryDirs() = %v, want exactly [%q]", dirs, repoDir)
	}
}

func TestRepositoryKeyMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		repoName string
		liveKey  string
		want     bool
	}{
		{"github.com/owner/repo", "owner/repo", true},
		{"github.com/owner/repo", "github.com/owner/repo", true},
		{"owner/repo", "github.com/owner/repo", true},
		{"github.com/owner/repo", "owner/other", false},
		{"github.com/owner/repo", "repo", true},
		{"github.com/owner/repo", "", false},
		{"", "owner/repo", false},
		{"github.com/owner/repository", "owner/repo", false},
	}

	for _, tc := range tests {
		if got := repositoryKeyMatches(tc.repoName, tc.liveKey); got != tc.want {
			t.Errorf("repositoryKeyMatches(%q, %q) = %v, want %v", tc.repoName, tc.liveKey, got, tc.want)
		}
	}
}
