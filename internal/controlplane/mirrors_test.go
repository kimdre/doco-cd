package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/source/store"
)

var testMirrors = []store.Mirror{
	{Repository: "github.com/owner/a", Path: "/data/github.com/owner/a/mirror"},
	{Repository: "github.com/owner/b", Path: "/data/github.com/owner/a/submodules/1"},
	{Repository: "github.com/owner/b", Path: "/data/github.com/owner/b/mirror"},
}

type compactCall struct {
	path string
	opts git.MirrorCompactOptions
}

// recordingCompactor records its calls and returns the results by mirror path,
// "compacted" for any other mirror.
type recordingCompactor struct {
	mu      sync.Mutex
	calls   []compactCall
	results map[string]git.MirrorCompaction
	errs    map[string]error
}

func (r *recordingCompactor) compact(_ context.Context, _ *slog.Logger, path string, opts git.MirrorCompactOptions) (git.MirrorCompaction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls = append(r.calls, compactCall{path: path, opts: opts})

	result, ok := r.results[path]
	if !ok {
		result = git.MirrorCompaction{
			Result: git.MirrorCompactionCompacted, SizeBytes: 100,
			SizeBytesBefore: 300,
		}
	}

	return result, r.errs[path]
}

func newMirrorCompactionTestRuns(t *testing.T, compact func(context.Context, *slog.Logger, string, git.MirrorCompactOptions) (git.MirrorCompaction, error)) (*Runs, *deploymentRunTracker) {
	t.Helper()

	tracker := newDeploymentRunTracker(nil)
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		tracker:    tracker,
		storageDir: "/data",
		listMirrors: func(dir string) ([]store.Mirror, error) {
			if dir != "/data" {
				t.Errorf("listMirrors(%q), want /data", dir)
			}

			return slices.Clone(testMirrors), nil
		},
		compactMirror: compact,
	})

	return runs, tracker
}

func TestTriggerMirrorCompactionSummarizesResults(t *testing.T) {
	t.Parallel()

	compactor := &recordingCompactor{results: map[string]git.MirrorCompaction{
		testMirrors[1].Path: {
			MirrorPackStats: git.MirrorPackStats{Result: git.MirrorCompactionSkippedBusy, SizeBytes: -1},
			SizeBytesBefore: -1,
		},
		testMirrors[2].Path: {
			MirrorPackStats: git.MirrorPackStats{Result: git.MirrorCompactionSkippedSize, SizeBytes: 3 << 20},
			SizeBytesBefore: 3 << 20,
		},
	}}
	runs, tracker := newMirrorCompactionTestRuns(t, compactor.compact)

	jobID, err := runs.TriggerMirrorCompaction(t.Context(), "", MirrorCompactionRequest{}, true)
	if err != nil {
		t.Fatalf("TriggerMirrorCompaction() error = %v", err)
	}

	run, ok := tracker.Get(jobID)
	if !ok {
		t.Fatalf("run %q not tracked", jobID)
	}

	want := "repack of 3 mirrors: 1 compacted, 1 skipped_size, 1 skipped_busy; packfiles 3.0 MiB -> 3.0 MiB"
	if run.Trigger != RunTriggerMirrorCompaction || run.Status != RunStatusSucceeded || run.Message != want || run.Repository != "" {
		t.Fatalf("run = %#v, want succeeded %s run with message %q", run, RunTriggerMirrorCompaction, want)
	}

	if len(compactor.calls) != len(testMirrors) {
		t.Fatalf("compacted %d mirrors, want %d", len(compactor.calls), len(testMirrors))
	}

	for i, call := range compactor.calls {
		want := git.MirrorCompactOptions{
			Repository:   testMirrors[i].Repository,
			Mode:         git.MirrorCompactionRepack,
			MaxSizeBytes: DefaultMirrorCompactionMaxSize,
		}
		if call.path != testMirrors[i].Path || call.opts != want {
			t.Errorf("call %d = %+v, want %s with %+v", i, call, testMirrors[i].Path, want)
		}
	}
}

func TestTriggerMirrorCompactionFiltersByRepository(t *testing.T) {
	t.Parallel()

	compactor := &recordingCompactor{}
	runs, tracker := newMirrorCompactionTestRuns(t, compactor.compact)

	maxSize := int64(0)

	// A clone URL selects the same mirrors as the repository name.
	jobID, err := runs.TriggerMirrorCompaction(t.Context(), "job-1", MirrorCompactionRequest{
		Repository:   "https://github.com/owner/b.git",
		Mode:         git.MirrorCompactionCopy,
		MaxSizeBytes: &maxSize,
	}, true)
	if err != nil || jobID != "job-1" {
		t.Fatalf("TriggerMirrorCompaction() = %q, %v, want job-1", jobID, err)
	}

	paths := make([]string, 0, len(compactor.calls))
	for _, call := range compactor.calls {
		paths = append(paths, call.path)

		if call.opts.Mode != git.MirrorCompactionCopy || call.opts.MaxSizeBytes != 0 {
			t.Errorf("options = %+v, want copy without size limit", call.opts)
		}
	}

	if want := []string{testMirrors[1].Path, testMirrors[2].Path}; !slices.Equal(paths, want) {
		t.Fatalf("compacted %v, want %v", paths, want)
	}

	run, _ := tracker.Get(jobID)
	if run.Repository != "github.com/owner/b" || !strings.HasPrefix(run.Message, "copy of 2 mirrors: 2 compacted;") {
		t.Fatalf("run = %#v, want copy of the 2 mirrors of github.com/owner/b", run)
	}

	_, err = runs.TriggerMirrorCompaction(t.Context(), "", MirrorCompactionRequest{Repository: "github.com/owner/missing"}, true)
	if !errors.Is(err, ErrNoMirrors) {
		t.Fatalf("TriggerMirrorCompaction(missing) error = %v, want ErrNoMirrors", err)
	}
}

func TestTriggerMirrorCompactionRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	compactor := &recordingCompactor{}
	runs, tracker := newMirrorCompactionTestRuns(t, compactor.compact)

	negative := int64(-1)

	for _, req := range []MirrorCompactionRequest{
		{Mode: "gc"},
		{MaxSizeBytes: &negative},
	} {
		jobID, err := runs.TriggerMirrorCompaction(t.Context(), "", req, true)
		if !errors.Is(err, ErrInvalidMirrorCompactionRequest) {
			t.Errorf("TriggerMirrorCompaction(%+v) error = %v, want ErrInvalidMirrorCompactionRequest", req, err)
		}

		if _, ok := tracker.Get(jobID); ok {
			t.Errorf("TriggerMirrorCompaction(%+v) tracked a run", req)
		}
	}

	if len(compactor.calls) != 0 {
		t.Fatalf("compacted %d mirrors, want none", len(compactor.calls))
	}

	// Without a data directory there is nothing to compact.
	empty := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})
	if _, err := empty.TriggerMirrorCompaction(t.Context(), "", MirrorCompactionRequest{}, true); !errors.Is(err, ErrNoMirrors) {
		t.Fatalf("TriggerMirrorCompaction() without storage error = %v, want ErrNoMirrors", err)
	}
}

func TestTriggerMirrorCompactionStatus(t *testing.T) {
	t.Parallel()

	skipped := git.MirrorCompaction{
		Result: git.MirrorCompactionSkippedSinglePack, SizeBytes: 10,
		SizeBytesBefore: 10,
	}
	failed := git.MirrorCompaction{
		Result: git.MirrorCompactionFailed, SizeBytes: -1,
		SizeBytesBefore: -1,
	}
	errBroken := errors.New("broken pack")

	for _, testCase := range []struct {
		name    string
		results map[string]git.MirrorCompaction
		errs    map[string]error
		status  RunStatus
		message string
	}{
		{
			name: "all skipped",
			results: map[string]git.MirrorCompaction{
				testMirrors[0].Path: skipped, testMirrors[1].Path: skipped, testMirrors[2].Path: skipped,
			},
			status:  RunStatusSkipped,
			message: "repack of 3 mirrors: 3 skipped_single_pack; packfiles 30 B -> 30 B",
		},
		{
			name:    "one failed",
			results: map[string]git.MirrorCompaction{testMirrors[1].Path: failed},
			errs:    map[string]error{testMirrors[1].Path: errBroken},
			status:  RunStatusFailed,
			message: "repack of 3 mirrors: 2 compacted, 1 failed; packfiles 600 B -> 200 B",
		},
		{
			// A compactor that reports no result for an error still counts as failed.
			name:    "unreported failure",
			results: map[string]git.MirrorCompaction{testMirrors[1].Path: {SizeBytesBefore: -1}},
			errs:    map[string]error{testMirrors[1].Path: errBroken},
			status:  RunStatusFailed,
			message: "repack of 3 mirrors: 2 compacted, 1 failed; packfiles 600 B -> 200 B",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			compactor := &recordingCompactor{results: testCase.results, errs: testCase.errs}
			runs, tracker := newMirrorCompactionTestRuns(t, compactor.compact)

			jobID, err := runs.TriggerMirrorCompaction(t.Context(), "", MirrorCompactionRequest{}, true)

			if testCase.status == RunStatusFailed {
				failedErr, ok := errors.AsType[*MirrorCompactionsFailedError](err)
				if !ok || failedErr.Failed != 1 || failedErr.Total != 3 || !errors.Is(err, errBroken) {
					t.Fatalf("TriggerMirrorCompaction() error = %v, want 1/3 failed caused by %v", err, errBroken)
				}
			} else if err != nil {
				t.Fatalf("TriggerMirrorCompaction() error = %v", err)
			}

			run, _ := tracker.Get(jobID)
			if run.Status != testCase.status || run.Message != testCase.message {
				t.Fatalf("run = %s %q, want %s %q", run.Status, run.Message, testCase.status, testCase.message)
			}
		})
	}
}

func TestTriggerMirrorCompactionAllowsOneRunAtATime(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	unblock := make(chan struct{})

	var calls atomic.Int32

	runs, tracker := newMirrorCompactionTestRuns(t, func(context.Context, *slog.Logger, string, git.MirrorCompactOptions) (git.MirrorCompaction, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-unblock
		}

		return git.MirrorCompaction{Result: git.MirrorCompactionCompacted}, nil
	})

	firstJobID, err := runs.TriggerMirrorCompaction(t.Context(), "", MirrorCompactionRequest{}, false)
	if err != nil {
		t.Fatalf("TriggerMirrorCompaction() error = %v", err)
	}

	<-started

	jobID, err := runs.TriggerMirrorCompaction(t.Context(), "", MirrorCompactionRequest{Mode: git.MirrorCompactionCopy}, false)

	activeErr, ok := errors.AsType[*MirrorCompactionActiveError](err)
	if !ok || activeErr.JobID != firstJobID || jobID != firstJobID {
		t.Fatalf("concurrent TriggerMirrorCompaction() = %q, %v, want the active run %q", jobID, err, firstJobID)
	}

	close(unblock)
	waitForDeploymentRunStatus(t, tracker, firstJobID, RunStatusSucceeded)

	if _, err := runs.TriggerMirrorCompaction(t.Context(), "", MirrorCompactionRequest{}, true); err != nil {
		t.Fatalf("TriggerMirrorCompaction() after the first run error = %v", err)
	}
}

func TestDrainCancelsMirrorCompaction(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})

	var calls atomic.Int32

	runs, tracker := newMirrorCompactionTestRuns(t, func(ctx context.Context, _ *slog.Logger, path string, _ git.MirrorCompactOptions) (git.MirrorCompaction, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()

		return git.MirrorCompaction{
			Path: path, Result: git.MirrorCompactionCancelled, SizeBytes: 50,
			SizeBytesBefore: 50,
		}, ctx.Err()
	})

	jobID, err := runs.TriggerMirrorCompaction(t.Context(), "", MirrorCompactionRequest{}, false)
	if err != nil {
		t.Fatalf("TriggerMirrorCompaction() error = %v", err)
	}

	<-started
	runs.Drain()

	run := waitForDeploymentRunStatus(t, tracker, jobID, RunStatusFailed)
	if want := "repack of 3 mirrors: 3 cancelled; packfiles 50 B -> 50 B"; run.Message != want {
		t.Fatalf("run message = %q, want %q", run.Message, want)
	}

	// The mirrors after the cancelled one are not attempted.
	if got := calls.Load(); got != 1 {
		t.Fatalf("compacted %d mirrors, want 1", got)
	}

	if runs.storage.activeCompaction != "" {
		t.Fatalf("active compaction = %q after the run, want none", runs.storage.activeCompaction)
	}
}

func TestTriggerMirrorCompactionWaitReportsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	runs, _ := newMirrorCompactionTestRuns(t, func(context.Context, *slog.Logger, string, git.MirrorCompactOptions) (git.MirrorCompaction, error) {
		cancel()

		return git.MirrorCompaction{Result: git.MirrorCompactionCompacted}, nil
	})

	_, err := runs.TriggerMirrorCompaction(ctx, "", MirrorCompactionRequest{}, true)
	if !IsLifecycleCancellation(err) {
		t.Fatalf("TriggerMirrorCompaction() error = %v, want a lifecycle cancellation", err)
	}
}

func TestTriggerMirrorCompactionReleasesRejectedRun(t *testing.T) {
	t.Parallel()

	compactor := &recordingCompactor{}
	runs, _ := newMirrorCompactionTestRuns(t, compactor.compact)
	runs.Drain()

	for _, wait := range []bool{false, true} {
		_, err := runs.TriggerMirrorCompaction(t.Context(), "", MirrorCompactionRequest{}, wait)
		if !IsLifecycleCancellation(err) {
			t.Fatalf("TriggerMirrorCompaction(wait=%t) after Drain() error = %v, want a lifecycle cancellation", wait, err)
		}

		if runs.storage.activeCompaction != "" {
			t.Fatalf("active compaction = %q after a rejected run, want none", runs.storage.activeCompaction)
		}
	}
}

func TestTriggerMirrorCompactionReportsUninspectableMirrors(t *testing.T) {
	t.Parallel()

	errConfig := errors.New("parse mirror configuration")
	mirrors := []store.Mirror{
		{Path: "/data/github.com/owner/a/submodules/broken", Err: errConfig},
		{Repository: "github.com/owner/a", Path: "/data/github.com/owner/a/mirror"},
		{Repository: "github.com/owner/b", Path: "/data/github.com/owner/b/mirror", Err: errConfig},
	}

	compactor := &recordingCompactor{}
	tracker := newDeploymentRunTracker(nil)
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		tracker:       tracker,
		storageDir:    "/data",
		listMirrors:   func(string) ([]store.Mirror, error) { return slices.Clone(mirrors), nil },
		compactMirror: compactor.compact,
	})

	// A broken mirror fails on its own instead of blocking the others.
	jobID, err := runs.TriggerMirrorCompaction(t.Context(), "", MirrorCompactionRequest{}, true)

	var failed *MirrorCompactionsFailedError
	if !errors.As(err, &failed) || failed.Failed != 2 || failed.Total != 3 || !errors.Is(err, errConfig) {
		t.Fatalf("TriggerMirrorCompaction() error = %v, want 2/3 failed with the configuration error", err)
	}

	if len(compactor.calls) != 1 || compactor.calls[0].path != mirrors[1].Path {
		t.Fatalf("compacted %+v, want only %s", compactor.calls, mirrors[1].Path)
	}

	run, _ := tracker.Get(jobID)
	if want := "repack of 3 mirrors: 1 compacted, 2 failed; packfiles 300 B -> 100 B"; run.Status != RunStatusFailed || run.Message != want {
		t.Fatalf("run = %#v, want failed run with message %q", run, want)
	}

	// A broken mirror still matches the repository its store is named after.
	_, err = runs.TriggerMirrorCompaction(t.Context(), "", MirrorCompactionRequest{Repository: "github.com/owner/b"}, true)
	if !errors.As(err, &failed) || failed.Failed != 1 || failed.Total != 1 || !errors.Is(err, errConfig) {
		t.Fatalf("TriggerMirrorCompaction(github.com/owner/b) error = %v, want 1/1 failed with the configuration error", err)
	}

	if len(compactor.calls) != 1 {
		t.Fatalf("compacted %+v, want no further calls", compactor.calls)
	}
}

func TestTriggerMirrorCompactionWithoutMirrors(t *testing.T) {
	t.Parallel()

	// The store listing finds nothing in a data directory that does not exist yet.
	dataDir := filepath.Join(t.TempDir(), "data")

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{storageDir: dataDir})
	if _, err := runs.TriggerMirrorCompaction(t.Context(), "", MirrorCompactionRequest{}, true); !errors.Is(err, ErrNoMirrors) {
		t.Fatalf("TriggerMirrorCompaction() error = %v, want ErrNoMirrors", err)
	}
}

func TestFormatBytes(t *testing.T) {
	t.Parallel()

	for n, want := range map[int64]string{
		0:                    "0 B",
		1023:                 "1023 B",
		1024:                 "1.0 KiB",
		11534336:             "11.0 MiB",
		3 << 30:              "3.0 GiB",
		1<<40 + 1<<39 + 1000: "1.5 TiB",
	} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
