package prometheus

import (
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

	gitInternal "github.com/kimdre/doco-cd/internal/git"
)

func TestGitMirrorPacksReportedAfterFetch(t *testing.T) {
	t.Parallel()

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

	cloneURL := "file://" + originPath
	mirrorPath := filepath.Join(t.TempDir(), "mirror")

	// The first call clones; only the second one fetches into the mirror.
	for range 2 {
		if _, err := gitInternal.CloneOrUpdateBareMirror(nil, cloneURL, gitInternal.MainBranch, mirrorPath,
			false, "", "", "", false, transport.ProxyOptions{}, 0); err != nil {
			t.Fatalf("CloneOrUpdateBareMirror() error = %v", err)
		}
	}

	if got := testutil.ToFloat64(GitMirrorPacks.WithLabelValues(gitInternal.GetRepoName(cloneURL))); got != 1 {
		t.Fatalf("git_mirror_packs = %v, want 1", got)
	}
}

func TestGitMirrorCompactionMetricsAreRegistered(t *testing.T) {
	t.Parallel()

	GitMirrorCompactionsTotal.WithLabelValues("example.com/owner/registered", gitInternal.MirrorCompactionCompacted).Inc()
	GitMirrorCompactionDuration.WithLabelValues("example.com/owner/registered").Observe(0.1)

	metricFamilies, err := clientPrometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	metricNames := make([]string, 0, len(metricFamilies))
	for _, metricFamily := range metricFamilies {
		metricNames = append(metricNames, metricFamily.GetName())
	}

	for _, expectedName := range []string{
		"doco_cd_git_mirror_compactions_total",
		"doco_cd_git_mirror_compaction_duration_seconds",
	} {
		if !slices.Contains(metricNames, expectedName) {
			t.Errorf("expected gathered metrics to contain %q", expectedName)
		}
	}
}
