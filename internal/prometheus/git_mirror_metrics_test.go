package prometheus

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	clientPrometheus "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	gitInternal "github.com/kimdre/doco-cd/internal/git"
)

// newMetricsOrigin creates a repository with one commit and returns its clone URL.
func newMetricsOrigin(t *testing.T) string {
	t.Helper()

	originPath := filepath.Join(t.TempDir(), "origin")

	origin, err := gogit.PlainInit(originPath, false)
	if err != nil {
		t.Fatalf("init origin: %v", err)
	}

	if err := origin.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}

	if err := os.WriteFile(filepath.Join(originPath, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	wt, err := origin.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("add: %v", err)
	}

	if _, err := wt.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "metrics-test", Email: "metrics-test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	return "file://" + originPath
}

// setupMetricsMirror creates a bare mirror of a repository with one commit
// that has been fetched once, and returns the clone URL and mirror path.
func setupMetricsMirror(t *testing.T) (string, string) {
	t.Helper()

	cloneURL := newMetricsOrigin(t)
	mirrorPath := filepath.Join(t.TempDir(), "mirror")

	// The first call clones; only the second one fetches into the mirror.
	for range 2 {
		if _, err := gitInternal.CloneOrUpdateBareMirror(nil, cloneURL, gitInternal.MainBranch, mirrorPath,
			false, "", "", "", false, transport.ProxyOptions{}, 0); err != nil {
			t.Fatalf("CloneOrUpdateBareMirror() error = %v", err)
		}
	}

	return cloneURL, mirrorPath
}

// storeLooseBlob writes a blob into the mirror at mirrorPath as a loose object.
func storeLooseBlob(t *testing.T, mirrorPath string) {
	t.Helper()

	repo, err := gogit.PlainOpen(mirrorPath)
	if err != nil {
		t.Fatalf("open mirror: %v", err)
	}

	obj := repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)

	w, err := obj.Writer()
	if err != nil {
		t.Fatalf("loose object writer: %v", err)
	}

	if _, err := w.Write([]byte("loose object\n")); err != nil {
		t.Fatalf("write loose object: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("close loose object: %v", err)
	}

	if _, err := repo.Storer.SetEncodedObject(obj); err != nil {
		t.Fatalf("store loose object: %v", err)
	}
}

func TestGitMirrorPacksReportedAfterCloneAndFetch(t *testing.T) {
	t.Parallel()

	cloneURL := newMetricsOrigin(t)
	mirrorPath := filepath.Join(t.TempDir(), "mirror")

	// The first call clones and the second one fetches into the mirror; both report it.
	for _, operation := range []string{"clone", "fetch"} {
		// Reset the gauges, so the fetch cannot pass on the values the clone reported.
		GitMirrorPacks.DeleteLabelValues(gitInternal.GetRepoName(cloneURL))
		GitMirrorSizeBytes.DeleteLabelValues(gitInternal.GetRepoName(cloneURL))

		if _, err := gitInternal.CloneOrUpdateBareMirror(nil, cloneURL, gitInternal.MainBranch, mirrorPath,
			false, "", "", "", false, transport.ProxyOptions{}, 0); err != nil {
			t.Fatalf("%s: CloneOrUpdateBareMirror() error = %v", operation, err)
		}

		if got := testutil.ToFloat64(GitMirrorPacks.WithLabelValues(gitInternal.GetRepoName(cloneURL))); got != 1 {
			t.Fatalf("%s: git_mirror_packs = %v, want 1", operation, got)
		}

		packs, err := filepath.Glob(filepath.Join(mirrorPath, "objects", "pack", "pack-*.pack"))
		if err != nil || len(packs) != 1 {
			t.Fatalf("%s: glob packs = %v, %v, want one pack", operation, packs, err)
		}

		info, err := os.Stat(packs[0])
		if err != nil {
			t.Fatalf("%s: stat pack: %v", operation, err)
		}

		if got := testutil.ToFloat64(GitMirrorSizeBytes.WithLabelValues(gitInternal.GetRepoName(cloneURL))); got != float64(info.Size()) {
			t.Fatalf("%s: git_mirror_size_bytes = %v, want %d", operation, got, info.Size())
		}
	}
}

func TestGitMirrorCompactionMetricsByMode(t *testing.T) {
	t.Parallel()

	cloneURL, mirrorPath := setupMetricsMirror(t)
	repository := gitInternal.GetRepoName(cloneURL)

	// Copy mode leaves a single pack alone. A loose object leaves repack
	// something to merge, so it replaces the pack regardless of its size.
	for _, mode := range []gitInternal.MirrorCompactionMode{gitInternal.MirrorCompactionCopy, gitInternal.MirrorCompactionRepack} {
		if mode == gitInternal.MirrorCompactionRepack {
			storeLooseBlob(t, mirrorPath)
		}

		if _, err := gitInternal.CompactMirror(t.Context(), slog.New(slog.DiscardHandler), mirrorPath,
			gitInternal.MirrorCompactOptions{Repository: repository, Mode: mode}); err != nil {
			t.Fatalf("CompactMirror(%s) error = %v", mode, err)
		}
	}

	for _, tc := range []struct {
		mode      gitInternal.MirrorCompactionMode
		result    string
		durations uint64
	}{
		{gitInternal.MirrorCompactionCopy, gitInternal.MirrorCompactionSkippedSinglePack, 0},
		{gitInternal.MirrorCompactionRepack, gitInternal.MirrorCompactionCompacted, 1},
	} {
		if got := testutil.ToFloat64(GitMirrorCompactionsTotal.WithLabelValues(repository, string(tc.mode), tc.result)); got != 1 {
			t.Errorf("git_mirror_compactions_total{mode=%q,result=%q} = %v, want 1", tc.mode, tc.result, got)
		}

		var metric dto.Metric
		if err := GitMirrorCompactionDuration.WithLabelValues(repository, string(tc.mode)).(clientPrometheus.Histogram).Write(&metric); err != nil {
			t.Fatalf("read compaction duration: %v", err)
		}

		if got := metric.GetHistogram().GetSampleCount(); got != tc.durations {
			t.Errorf("git_mirror_compaction_duration_seconds{mode=%q} count = %d, want %d", tc.mode, got, tc.durations)
		}
	}

	if got := testutil.ToFloat64(GitMirrorPacks.WithLabelValues(repository)); got != 1 {
		t.Errorf("git_mirror_packs = %v, want 1", got)
	}

	size := testutil.ToFloat64(GitMirrorSizeBytes.WithLabelValues(repository))

	// A mirror skipped because it is in use reports no packs, which must not reset the gauges.
	unlock := gitInternal.AcquireSharedMirrorLock(mirrorPath)

	result, err := gitInternal.CompactMirror(t.Context(), slog.New(slog.DiscardHandler), mirrorPath,
		gitInternal.MirrorCompactOptions{Repository: repository, Mode: gitInternal.MirrorCompactionRepack})

	unlock()

	if err != nil || result.Result != gitInternal.MirrorCompactionSkippedBusy {
		t.Fatalf("CompactMirror() = %+v, %v, want %q", result, err, gitInternal.MirrorCompactionSkippedBusy)
	}

	if got := testutil.ToFloat64(GitMirrorPacks.WithLabelValues(repository)); got != 1 {
		t.Errorf("git_mirror_packs after skipped compaction = %v, want 1", got)
	}

	if got := testutil.ToFloat64(GitMirrorSizeBytes.WithLabelValues(repository)); got != size || size <= 0 {
		t.Errorf("git_mirror_size_bytes after skipped compaction = %v, want %v", got, size)
	}
}

func TestGitMirrorMetricsAreRegistered(t *testing.T) {
	t.Parallel()

	GitMirrorSizeBytes.WithLabelValues("example.com/owner/registered").Set(1)
	GitMirrorCompactionsTotal.WithLabelValues("example.com/owner/registered", string(gitInternal.MirrorCompactionCopy),
		gitInternal.MirrorCompactionCompacted).Inc()
	GitMirrorCompactionDuration.WithLabelValues("example.com/owner/registered", string(gitInternal.MirrorCompactionCopy)).Observe(0.1)

	metricFamilies, err := clientPrometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	metricNames := make([]string, 0, len(metricFamilies))
	for _, metricFamily := range metricFamilies {
		metricNames = append(metricNames, metricFamily.GetName())
	}

	for _, expectedName := range []string{
		"doco_cd_git_mirror_size_bytes",
		"doco_cd_git_mirror_compactions_total",
		"doco_cd_git_mirror_compaction_duration_seconds",
	} {
		if !slices.Contains(metricNames, expectedName) {
			t.Errorf("expected gathered metrics to contain %q", expectedName)
		}
	}
}
