package gc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/source"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
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
		listRepos:      store.ListRepositoryDirs,
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

	s.sweepRepoDir(context.Background(), repoDir, nil)

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

	s.sweepRepoDir(context.Background(), repoDir, live)

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

	s.sweepRepoDir(context.Background(), repoDir, nil)

	if _, err := os.Stat(artifactPath); err != nil {
		t.Fatalf("in-flight artifact was removed unexpectedly, stat err = %v", err)
	}
}

func TestSweeper_SweepRepoDir_ReportsMetrics(t *testing.T) {
	t.Parallel()

	s, dataMountPoint := newTestSweeper(t)

	// Metrics are process-global, so the repository name is unique to this test.
	const repoName = "github.com/owner/metrics-repo"

	repoDir := filepath.Join(dataMountPoint, filepath.FromSlash(repoName))
	expiredPath := makeArtifactDir(t, repoDir, "expired")
	makeArtifactDir(t, repoDir, "fresh")

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(expiredPath, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// The counter survives repeated runs of this test (go test -count=N), so only its increase is checked.
	removedBefore := testutil.ToFloat64(prometheus.ArtifactGCRemovedTotal.WithLabelValues(repoName))

	s.sweepRepoDir(context.Background(), repoDir, nil)

	if got := testutil.ToFloat64(prometheus.ArtifactGCRemovedTotal.WithLabelValues(repoName)) - removedBefore; got != 1 {
		t.Errorf("artifact_gc_removed_total increased by %v, want 1", got)
	}

	if got := testutil.ToFloat64(prometheus.ArtifactGCKept.WithLabelValues(repoName)); got != 1 {
		t.Errorf("artifact_gc_kept = %v, want 1", got)
	}
}

func TestSweeper_Sweep_DropsMetricsOfRemovedRepositories(t *testing.T) {
	t.Parallel()

	s, dataMountPoint := newTestSweeper(t)

	// Metrics are process-global, so the repository names are unique to this test.
	const (
		removedRepo   = "github.com/owner/gc-metrics-removed-repo"
		remainingRepo = "github.com/owner/gc-metrics-remaining-repo"
	)

	for _, repoName := range []string{removedRepo, remainingRepo} {
		makeArtifactDir(t, filepath.Join(dataMountPoint, filepath.FromSlash(repoName)), "fresh")
	}

	s.sweep(context.Background())

	if err := os.RemoveAll(filepath.Join(dataMountPoint, filepath.FromSlash(removedRepo))); err != nil {
		t.Fatalf("remove repository dir: %v", err)
	}

	s.sweep(context.Background())

	// DeleteLabelValues reports whether the series existed, which makes it the
	// presence check here; deleting is harmless since the test ends with it.
	if prometheus.ArtifactGCKept.DeleteLabelValues(removedRepo) {
		t.Error("artifact_gc_kept still reports the removed repository")
	}

	if prometheus.ArtifactGCRemovedTotal.DeleteLabelValues(removedRepo) {
		t.Error("artifact_gc_removed_total still reports the removed repository")
	}

	if !prometheus.ArtifactGCKept.DeleteLabelValues(remainingRepo) {
		t.Error("artifact_gc_kept no longer reports the remaining repository")
	}

	if !prometheus.ArtifactGCRemovedTotal.DeleteLabelValues(remainingRepo) {
		t.Error("artifact_gc_removed_total no longer reports the remaining repository")
	}
}

func TestSweeper_SweepRepoDir_LeavesKeptMetricWhenSweepCannotList(t *testing.T) {
	t.Parallel()

	s, dataMountPoint := newTestSweeper(t)

	// Metrics are process-global, so the repository name is unique to this test.
	const repoName = "github.com/owner/unlistable-repo"

	repoDir := filepath.Join(dataMountPoint, filepath.FromSlash(repoName))
	makeArtifactDir(t, repoDir, "kept")

	prometheus.ArtifactGCKept.WithLabelValues(repoName).Set(3)

	s.sweepDirectory = func(string, set.Set[store.Revision], store.GCOptions, time.Time) (store.GCResult, error) {
		return store.GCResult{}, errors.New("list artifacts: permission denied")
	}

	s.sweepRepoDir(context.Background(), repoDir, nil)

	if got := testutil.ToFloat64(prometheus.ArtifactGCKept.WithLabelValues(repoName)); got != 3 {
		t.Errorf("artifact_gc_kept = %v, want the previous value 3", got)
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

func TestSweeper_Sweep_LogsAndSkipsRepositoryInUse(t *testing.T) {
	t.Parallel()

	s, dataMountPoint := newTestSweeper(t)

	logs := &bytes.Buffer{}
	s.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	repoDir := filepath.Join(dataMountPoint, "github.com", "owner", "busy-repo")
	artifactPath := makeArtifactDir(t, repoDir, "expired")

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(artifactPath, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// A deployment of the repository holds its shared GC lock.
	unlock, err := sourcecache.AcquireSharedGCPathLock(repoDir)
	if err != nil {
		t.Fatalf("AcquireSharedGCPathLock: %v", err)
	}

	defer unlock()

	s.sweep(context.Background())

	if _, err := os.Stat(artifactPath); err != nil {
		t.Fatalf("artifact of a repository in use was removed, stat err = %v", err)
	}

	var record map[string]any

	for line := range bytes.Lines(logs.Bytes()) {
		var r map[string]any
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("decode log record %q: %v", line, err)
		}

		if r["msg"] == "gc: repository is in use by a deployment or scheduled run; skipping it until the next sweep" {
			record = r
		}
	}

	if record == nil {
		t.Fatalf("log records = %s, want one for the repository in use", logs.String())
	}

	if record["level"] != "DEBUG" || record["repository"] != repoDir || record["next_sweep_in"] != "1h0m0s" {
		t.Fatalf("log record = %v, want level DEBUG, repository %s and next_sweep_in 1h0m0s", record, repoDir)
	}
}

func TestSweeper_Start_NoopWhenUnconfigured(t *testing.T) {
	t.Parallel()

	// Must return immediately without panicking when required fields are
	// missing, mirroring internal/certrotation.Watcher's own guard.
	(&Sweeper{}).Start(context.Background())
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

// newEvictionTestSource creates a complete, unused source store - mirror, artifact and live files - whose last use was
// recorded lastUsedAgo ago, and returns its directory.
func newEvictionTestSource(t *testing.T, dataMountPoint, repoName string, lastUsedAgo time.Duration) string {
	t.Helper()

	repoDir := filepath.Join(dataMountPoint, filepath.FromSlash(repoName))
	makeArtifactDir(t, repoDir, "rev")

	for _, dir := range []string{store.MirrorSubdir, filepath.Join(store.LiveSubdir, "default", "stack", "root")} {
		if err := os.MkdirAll(filepath.Join(repoDir, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	if err := sourcecache.TouchLastUsed(repoDir); err != nil {
		t.Fatalf("TouchLastUsed: %v", err)
	}

	lastUsed := time.Now().Add(-lastUsedAgo)
	if err := os.Chtimes(repoDir+".last-used", lastUsed, lastUsed); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	return repoDir
}

func newEvictionTestSweeper(t *testing.T) (*Sweeper, string, *[]string) {
	t.Helper()

	s, dataMountPoint := newTestSweeper(t)
	s.sourceTTL = time.Hour

	var checked []string

	s.referencedBy = func(_ context.Context, repoDir string) ([]string, error) {
		checked = append(checked, repoDir)
		return nil, nil
	}

	return s, dataMountPoint, &checked
}

func TestSweeper_Sweep_EvictsUnusedSource(t *testing.T) {
	t.Parallel()

	s, dataMountPoint, checked := newEvictionTestSweeper(t)
	repoDir := newEvictionTestSource(t, dataMountPoint, "github.com/owner/evict-unused", 2*time.Hour)

	s.sweep(context.Background())

	if _, err := os.Stat(repoDir); !os.IsNotExist(err) {
		t.Fatalf("unused source still exists, stat err = %v", err)
	}

	for _, path := range []string{repoDir + store.EvictingSuffix, repoDir + ".last-used"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s still exists after eviction, stat err = %v", path, err)
		}
	}

	// The GC gate's lock file must outlive the store it guards.
	if _, err := os.Stat(repoDir + ".gc-use.lock"); err != nil {
		t.Errorf("GC lock file was removed with the source, stat err = %v", err)
	}

	if len(*checked) != 1 || (*checked)[0] != repoDir {
		t.Errorf("reference check ran for %v, want exactly [%s]", *checked, repoDir)
	}
}

func TestSweeper_Sweep_KeepsSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		lastUsedAgo time.Duration
		// prepare adjusts the sweeper or the environment and returns a cleanup function.
		prepare func(t *testing.T, s *Sweeper, repoName string) func()
	}{
		{
			name:        "used recently",
			lastUsedAgo: 10 * time.Minute,
		},
		{
			name:        "source removal disabled",
			lastUsedAgo: 2 * time.Hour,
			prepare: func(_ *testing.T, s *Sweeper, _ string) func() {
				s.sourceTTL = 0
				return func() {}
			},
		},
		{
			name:        "revision deployed",
			lastUsedAgo: 2 * time.Hour,
			prepare: func(_ *testing.T, s *Sweeper, repoName string) func() {
				s.liveRevisions = func(context.Context, *docker.ContextRegistry, *slog.Logger, string, string) (map[string]set.Set[store.Revision], error) {
					return map[string]set.Set[store.Revision]{repoName: set.New[store.Revision]("other-rev")}, nil
				}

				return func() {}
			},
		},
		{
			name:        "deployment in progress",
			lastUsedAgo: 2 * time.Hour,
			prepare: func(_ *testing.T, _ *Sweeper, repoName string) func() {
				return source.MarkInFlight(repoName, "not-yet-published")
			},
		},
		{
			name:        "working directory referenced",
			lastUsedAgo: 2 * time.Hour,
			prepare: func(_ *testing.T, s *Sweeper, _ string) func() {
				s.referencedBy = func(context.Context, string) ([]string, error) {
					return []string{"legacy-container"}, nil
				}

				return func() {}
			},
		},
		{
			name:        "reference check failed",
			lastUsedAgo: 2 * time.Hour,
			prepare: func(_ *testing.T, s *Sweeper, _ string) func() {
				s.referencedBy = func(context.Context, string) ([]string, error) {
					return nil, errors.New("context unavailable")
				}

				return func() {}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, dataMountPoint, _ := newEvictionTestSweeper(t)

			// In-flight records are process-global, so every case uses its own repository name.
			repoName := "github.com/owner/keep-" + filepath.Base(t.Name())
			repoDir := newEvictionTestSource(t, dataMountPoint, repoName, tc.lastUsedAgo)

			if tc.prepare != nil {
				defer tc.prepare(t, s, repoName)()
			}

			s.sweep(context.Background())

			if _, err := os.Stat(filepath.Join(repoDir, store.MirrorSubdir)); err != nil {
				t.Fatalf("source was removed, stat err = %v", err)
			}
		})
	}
}

// OCI store names can nest, e.g. "registry/group/app/config" inside the store of "registry/group/app".
func TestSweeper_Sweep_KeepsSourceContainingNestedStore(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		dirs  []string
		files []string
	}{
		"nested store":                      {dirs: []string{filepath.Join("config", store.ArtifactsSubdir, "rev")}},
		"deeper nested store":               {dirs: []string{filepath.Join("group", "config", store.MirrorSubdir)}},
		"GC gate of a removed nested store": {files: []string{"config.gc-use.lock"}},
		"use record of a nested store":      {dirs: []string{"group"}, files: []string{filepath.Join("group", "config.last-used")}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s, dataMountPoint, _ := newEvictionTestSweeper(t)
			repoDir := newEvictionTestSource(t, dataMountPoint, "registry.example.com/group/app", 2*time.Hour)

			for _, dir := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(repoDir, dir), 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
			}

			for _, file := range tc.files {
				if err := os.WriteFile(filepath.Join(repoDir, file), nil, 0o600); err != nil {
					t.Fatalf("write %s: %v", file, err)
				}
			}

			s.sweep(context.Background())

			if _, err := os.Stat(filepath.Join(repoDir, store.MirrorSubdir)); err != nil {
				t.Fatalf("source containing another source was removed, stat err = %v", err)
			}
		})
	}
}

func TestSweeper_Sweep_StartsCountingForSourceWithoutUseRecord(t *testing.T) {
	t.Parallel()

	s, dataMountPoint, checked := newEvictionTestSweeper(t)
	repoDir := newEvictionTestSource(t, dataMountPoint, "github.com/owner/no-record", 2*time.Hour)

	if err := sourcecache.RemoveLastUsed(repoDir); err != nil {
		t.Fatalf("RemoveLastUsed: %v", err)
	}

	s.sweep(context.Background())

	if _, err := os.Stat(filepath.Join(repoDir, store.MirrorSubdir)); err != nil {
		t.Fatalf("source without a use record was removed, stat err = %v", err)
	}

	lastUsed, ok, err := sourcecache.LastUsed(repoDir)
	if err != nil || !ok {
		t.Fatalf("LastUsed() = %v, %v, %v; want a fresh record", lastUsed, ok, err)
	}

	if time.Since(lastUsed) > time.Minute {
		t.Errorf("use record is %s old, want it started now", time.Since(lastUsed))
	}

	if len(*checked) != 0 {
		t.Errorf("reference check ran for %v, want none", *checked)
	}
}

func TestSweeper_Sweep_RetriesFailedEviction(t *testing.T) {
	t.Parallel()

	s, dataMountPoint, _ := newEvictionTestSweeper(t)
	repoDir := newEvictionTestSource(t, dataMountPoint, "github.com/owner/retry-evict", 2*time.Hour)

	// A store an earlier eviction moved aside but failed to delete.
	staleTrash := filepath.Join(repoDir+store.EvictingSuffix, store.ArtifactsSubdir, "old")
	if err := os.MkdirAll(staleTrash, 0o755); err != nil {
		t.Fatalf("mkdir stale trash: %v", err)
	}

	dirs, err := store.ListRepositoryDirs(dataMountPoint)
	if err != nil {
		t.Fatalf("ListRepositoryDirs() error = %v", err)
	}

	if len(dirs) != 1 || dirs[0] != repoDir {
		t.Fatalf("ListRepositoryDirs() = %v, want only %s", dirs, repoDir)
	}

	s.sweep(context.Background())

	for _, path := range []string{repoDir, repoDir + store.EvictingSuffix} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s still exists after eviction, stat err = %v", path, err)
		}
	}
}
