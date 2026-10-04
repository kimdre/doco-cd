package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// DefaultMirrorCompactionMaxSize is the size above which a repack skips a
// mirror unless the request sets its own limit. The encoder holds several
// times the size of a mirror's packfiles in memory.
const DefaultMirrorCompactionMaxSize int64 = 256 << 20

var (
	// ErrInvalidMirrorCompactionRequest indicates an unknown mode or a negative size limit.
	ErrInvalidMirrorCompactionRequest = errors.New("invalid mirror compaction request")
	// ErrNoMirrors indicates that no Git mirror matches a compaction request.
	ErrNoMirrors = errors.New("no git mirrors found")
	// ErrMirrorCompactionPanicked is recorded when a mirror compaction run panics.
	ErrMirrorCompactionPanicked = errors.New("mirror compaction run panicked")
)

// mirrorCompactionResults orders the results in a compaction run's summary.
var mirrorCompactionResults = []string{
	git.MirrorCompactionCompacted,
	git.MirrorCompactionSkippedSinglePack,
	git.MirrorCompactionSkippedSize,
	git.MirrorCompactionSkippedBusy,
	git.MirrorCompactionFailed,
	git.MirrorCompactionCancelled,
}

// MirrorCompactionRequest selects the Git mirrors a compaction run processes.
type MirrorCompactionRequest struct {
	// Repository limits the run to the mirrors of one repository, given as
	// "<host>/<owner>/<repo>" or as a clone URL. Empty selects every mirror.
	Repository string
	// Mode defaults to git.MirrorCompactionRepack.
	Mode git.MirrorCompactionMode
	// MaxSizeBytes skips a repack of a mirror whose packfiles are larger. Nil
	// applies DefaultMirrorCompactionMaxSize, and zero disables the limit.
	MaxSizeBytes *int64
}

// options validates the request and applies its defaults.
func (r MirrorCompactionRequest) options() (git.MirrorCompactOptions, error) {
	opts := git.MirrorCompactOptions{
		Mode:         git.MirrorCompactionMode(strings.ToLower(strings.TrimSpace(string(r.Mode)))),
		MaxSizeBytes: DefaultMirrorCompactionMaxSize,
	}

	if opts.Mode == "" {
		opts.Mode = git.MirrorCompactionRepack
	}

	if !opts.Mode.Valid() {
		return opts, fmt.Errorf("%w: unknown mode %q, want %q or %q", ErrInvalidMirrorCompactionRequest,
			r.Mode, git.MirrorCompactionRepack, git.MirrorCompactionCopy)
	}

	if r.MaxSizeBytes != nil {
		if *r.MaxSizeBytes < 0 {
			return opts, fmt.Errorf("%w: max size %d is negative", ErrInvalidMirrorCompactionRequest, *r.MaxSizeBytes)
		}

		opts.MaxSizeBytes = *r.MaxSizeBytes
	}

	return opts, nil
}

// MirrorCompactionActiveError rejects a compaction request while another
// compaction run is active.
type MirrorCompactionActiveError struct {
	// JobID identifies the active run.
	JobID string
}

// Error names the active compaction run.
func (e *MirrorCompactionActiveError) Error() string {
	return fmt.Sprintf("mirror compaction %s is already running", e.JobID)
}

// MirrorCompactionsFailedError reports a compaction run in which mirrors
// failed or were cancelled.
type MirrorCompactionsFailedError struct {
	Failed int
	Total  int
	Cause  error
}

// Error reports the number of mirrors that were not compacted.
func (e *MirrorCompactionsFailedError) Error() string {
	return fmt.Sprintf("%d/%d mirror compactions failed", e.Failed, e.Total)
}

// Unwrap exposes the first failure, or the cancellation.
func (e *MirrorCompactionsFailedError) Unwrap() error {
	return e.Cause
}

type controlPlaneStorage struct {
	dir         string
	listMirrors func(dataDir string) ([]store.Mirror, error)
	compact     func(ctx context.Context, log *slog.Logger, path string, opts git.MirrorCompactOptions) (git.MirrorCompaction, error)

	mu sync.Mutex
	// activeCompaction is the job ID of the compaction run in progress, if any.
	activeCompaction string
}

func newControlPlaneStorage(dir string) *controlPlaneStorage {
	return &controlPlaneStorage{
		dir:         dir,
		listMirrors: store.ListMirrors,
		compact:     git.CompactMirror,
	}
}

// mirrors returns the mirrors of repository, or all mirrors if it is empty.
// A mirror that could not be inspected matches only if its repository is known.
func (s *controlPlaneStorage) mirrors(repository string) ([]store.Mirror, error) {
	if s.dir == "" {
		return nil, nil
	}

	mirrors, err := s.listMirrors(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list git mirrors: %w", err)
	}

	if repository == "" {
		return mirrors, nil
	}

	var matching []store.Mirror

	for _, mirror := range mirrors {
		if mirror.Repository == repository {
			matching = append(matching, mirror)
		}
	}

	return matching, nil
}

// claimCompaction makes jobID the active compaction run and returns the
// function that ends it, which is safe to call more than once. While another
// run is active, it returns that run's job ID instead.
func (s *controlPlaneStorage) claimCompaction(jobID string) (func(), string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeCompaction != "" {
		return nil, s.activeCompaction
	}

	s.activeCompaction = jobID

	return sync.OnceFunc(func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		s.activeCompaction = ""
	}), ""
}

// TriggerMirrorCompaction compacts the Git mirrors req selects, one at a time,
// in a tracked run. The run is cancelled when the runs are drained.
//
// It returns ErrInvalidMirrorCompactionRequest for an invalid request and
// ErrNoMirrors if no mirror matches. While another compaction run is active,
// it returns that run's job ID and a *MirrorCompactionActiveError. With wait,
// a run in which mirrors failed or were cancelled returns a
// *MirrorCompactionsFailedError.
func (c *Runs) TriggerMirrorCompaction(ctx context.Context, jobID string, req MirrorCompactionRequest, wait bool) (string, error) {
	if jobID == "" {
		jobID = id.New()
	}

	opts, err := req.options()
	if err != nil {
		return jobID, err
	}

	repository := strings.TrimSpace(req.Repository)
	if repository != "" {
		repository = git.GetRepoName(repository)
	}

	mirrors, err := c.storage.mirrors(repository)
	if err != nil {
		return jobID, err
	}

	if len(mirrors) == 0 {
		if repository != "" {
			return jobID, fmt.Errorf("%w for repository %s", ErrNoMirrors, repository)
		}

		return jobID, ErrNoMirrors
	}

	release, activeJobID := c.storage.claimCompaction(jobID)
	if release == nil {
		return activeJobID, &MirrorCompactionActiveError{JobID: activeJobID}
	}

	c.Accept(jobID, RunTriggerMirrorCompaction, RunMetadata{Repository: repository})

	jobLog := c.log.With(slog.String("job_id", jobID))

	mode := RunAsynchronous
	if wait {
		mode = RunSynchronous
	}

	err = c.Execute(ctx, jobID, RunExecution{
		Mode:         mode,
		PanicContext: "mirror compaction run",
		PanicError:   ErrMirrorCompactionPanicked,
	}, func(runCtx context.Context) (RunResult, error) {
		defer release()

		runCtx, cancel := context.WithCancel(runCtx)
		defer cancel()

		stopMaintenanceCancel := context.AfterFunc(c.maintenanceCtx, cancel) //nolint:contextcheck // Draining the runs cancels a compaction, which is safe to abort.
		defer stopMaintenanceCancel()

		return c.compactMirrors(runCtx, jobLog, mirrors, opts)
	})
	if err != nil {
		// The run never started if Execute rejected it.
		release()
	}

	return jobID, err
}

// compactMirrors compacts mirrors one after another and summarizes the results.
func (c *Runs) compactMirrors(ctx context.Context, log *slog.Logger, mirrors []store.Mirror, opts git.MirrorCompactOptions) (RunResult, error) {
	log.Info("compacting git mirrors",
		slog.String("mode", string(opts.Mode)),
		slog.Int("mirrors", len(mirrors)),
		slog.Int64("max_size_bytes", opts.MaxSizeBytes))

	start := time.Now()
	summary := newMirrorCompactionSummary(opts.Mode, len(mirrors))

	var cause error

	for i, mirror := range mirrors {
		if ctx.Err() != nil {
			summary.results[git.MirrorCompactionCancelled] += len(mirrors) - i

			break
		}

		opts.Repository = mirror.Repository

		if mirror.Err != nil {
			log.Warn("failed to inspect git mirror",
				slog.String("repository", mirror.Repository),
				slog.String("path", mirror.Path),
				slog.Any("error", mirror.Err))

			summary.add(git.MirrorCompaction{
				MirrorPackStats: git.MirrorPackStats{
					Repository:  mirror.Repository,
					Path:        mirror.Path,
					PacksBefore: -1,
					PacksAfter:  -1,
					SizeBytes:   -1,
					Mode:        opts.Mode,
					Result:      git.MirrorCompactionFailed,
				},
				SizeBytesBefore: -1,
			})

			if cause == nil {
				cause = mirror.Err
			}

			continue
		}

		result, err := c.storage.compact(ctx, log, mirror.Path, opts)
		if err != nil && result.Result == "" {
			result.Result = git.MirrorCompactionFailed
		}

		summary.add(result)

		if err != nil && cause == nil {
			cause = err
		}
	}

	// A cancellation explains the failures to the caller better than the first of them.
	if ctxErr := ctx.Err(); ctxErr != nil {
		cause = ctxErr
	}

	failed := summary.results[git.MirrorCompactionFailed] + summary.results[git.MirrorCompactionCancelled]

	log.Info("finished compacting git mirrors",
		slog.String("mode", string(opts.Mode)),
		slog.Group("mirrors",
			slog.Int("total", len(mirrors)),
			slog.Int("compacted", summary.results[git.MirrorCompactionCompacted]),
			slog.Int("skipped", len(mirrors)-summary.results[git.MirrorCompactionCompacted]-failed),
			slog.Int("failed", failed),
		),
		slog.Group("size_bytes",
			slog.Int64("before", summary.sizeBefore),
			slog.Int64("after", summary.sizeAfter),
		),
		slog.String("elapsed_time", time.Since(start).Truncate(time.Millisecond).String()))

	switch {
	case failed > 0:
		return FailedRun(summary.String()), &MirrorCompactionsFailedError{Failed: failed, Total: len(mirrors), Cause: cause}
	case summary.results[git.MirrorCompactionCompacted] == 0:
		return SkippedRun(summary.String()), nil
	default:
		return SucceededRun(summary.String()), nil
	}
}

// mirrorCompactionSummary counts the results of a compaction run and the
// combined size of the mirrors it measured.
type mirrorCompactionSummary struct {
	mode       git.MirrorCompactionMode
	total      int
	results    map[string]int
	sizeBefore int64
	sizeAfter  int64
}

func newMirrorCompactionSummary(mode git.MirrorCompactionMode, total int) *mirrorCompactionSummary {
	return &mirrorCompactionSummary{mode: mode, total: total, results: make(map[string]int)}
}

func (s *mirrorCompactionSummary) add(result git.MirrorCompaction) {
	s.results[result.Result]++

	if result.SizeBytesBefore >= 0 && result.SizeBytes >= 0 {
		s.sizeBefore += result.SizeBytesBefore
		s.sizeAfter += result.SizeBytes
	}
}

// String reports the results, e.g. "repack of 3 mirrors: 2 compacted,
// 1 skipped_busy; packfiles 20.5 MiB -> 11.0 MiB".
func (s *mirrorCompactionSummary) String() string {
	counts := make([]string, 0, len(s.results))

	for _, result := range mirrorCompactionResults {
		if n := s.results[result]; n > 0 {
			counts = append(counts, fmt.Sprintf("%d %s", n, result))
		}
	}

	mirrors := "mirrors"
	if s.total == 1 {
		mirrors = "mirror"
	}

	return fmt.Sprintf("%s of %d %s: %s; packfiles %s -> %s",
		s.mode, s.total, mirrors, strings.Join(counts, ", "), formatBytes(s.sizeBefore), formatBytes(s.sizeAfter))
}

// formatBytes formats n with a binary unit, e.g. "20.5 MiB".
func formatBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
