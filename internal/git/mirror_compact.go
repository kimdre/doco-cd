package git

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

const (
	// mirrorCompactPackThreshold is the packfile count above which a bare mirror
	// is consolidated into a single pack. Every fetch that brings new objects
	// writes another pack, and every freshly opened handle loads all pack indexes
	// on its first object read, so an unbounded pack count slows down every read.
	mirrorCompactPackThreshold = 32

	// mirrorCompactRetryDelay keeps a mirror whose compaction failed from
	// repeating the attempt, and its cost, on every poll.
	mirrorCompactRetryDelay = time.Hour

	tempPackPrefix = "tmp_pack_"
)

// Results reported in MirrorPackStats.Result when a compaction was attempted.
const (
	MirrorCompactionCompacted = "compacted"
	MirrorCompactionFailed    = "failed"
)

// MirrorPackStats describes a bare mirror's packfiles after a fetch and the
// compaction it triggered, if any.
type MirrorPackStats struct {
	// Repository is the mirrored repository in "<host>/<owner>/<repo>" form.
	// Several mirrors can report the same repository, e.g. a deployed
	// repository that is also included or used as a submodule elsewhere.
	Repository string
	// Path is the mirror's directory, which tells such mirrors apart.
	Path        string
	PacksBefore int
	PacksAfter  int
	// SizeBytes is the combined size of the packfiles left after any compaction,
	// or -1 if it could not be measured.
	SizeBytes int64
	// Result is empty when the pack count stayed within the compaction threshold.
	Result   string
	Duration time.Duration
}

// MirrorPackObserver receives MirrorPackStats after every bare mirror fetch.
type MirrorPackObserver func(MirrorPackStats)

var (
	mirrorPackObserver atomic.Pointer[MirrorPackObserver]

	// mirrorCompactionFailedAt maps mirror paths to their last failed compaction.
	mirrorCompactionFailedAt sync.Map
)

// SetMirrorPackObserver registers the observer that receives bare mirror pack
// statistics. Passing nil disables reporting.
func SetMirrorPackObserver(observer MirrorPackObserver) {
	if observer == nil {
		mirrorPackObserver.Store(nil)
		return
	}

	mirrorPackObserver.Store(&observer)
}

func notifyMirrorPackObserver(stats MirrorPackStats) {
	if observer := mirrorPackObserver.Load(); observer != nil {
		(*observer)(stats)
	}
}

// compactBareMirrorLocked consolidates the mirror's packfiles into a single pack
// once their count exceeds mirrorCompactPackThreshold. The caller must hold the
// mirror's exclusive lock.
//
// Compaction never fails the surrounding fetch: on any error the old packs are
// kept, since every object still lives in them. It reports whether the pack
// directory changed, in which case repo's cached pack index is stale and the
// caller must reopen the mirror before reading from it again.
func compactBareMirrorLocked(log *slog.Logger, repo *git.Repository, path, repository string) bool {
	storage, ok := repo.Storer.(*filesystem.Storage)
	if !ok {
		return false
	}

	packs, err := storage.ObjectPacks()
	if err != nil {
		log.Warn("failed to list bare mirror packfiles",
			slog.String("path", path),
			slog.Any("error", err))

		return false
	}

	packDir := filepath.Join(path, "objects", "pack")

	stats := MirrorPackStats{
		Repository:  repository,
		Path:        path,
		PacksBefore: len(packs),
		PacksAfter:  len(packs),
		SizeBytes:   -1,
	}

	// remaining lists the packs left after any compaction.
	remaining := packs

	defer func() {
		if size, err := packfilesSize(packDir, remaining); err == nil {
			stats.SizeBytes = size
		}

		notifyMirrorPackObserver(stats)
	}()

	if len(packs) <= mirrorCompactPackThreshold {
		return false
	}

	if failedAt, ok := mirrorCompactionFailedAt.Load(path); ok && time.Since(failedAt.(time.Time)) < mirrorCompactRetryDelay {
		return false
	}

	sizeBefore, err := packfilesSize(packDir, packs)
	if err != nil {
		stats.Result = MirrorCompactionFailed

		mirrorCompactionFailedAt.Store(path, time.Now())
		log.Warn("failed to measure bare mirror packfiles, skipping compaction",
			slog.String("path", path),
			slog.Group("packs",
				slog.Int("before", stats.PacksBefore),
			),
			slog.String("retry_delay", mirrorCompactRetryDelay.String()),
			slog.Any("error", err))

		return false
	}

	removeTempPacks(log, packDir)

	start := time.Now()
	result, err := consolidatePacks(storage, packDir, packs)
	stats.Duration = time.Since(start)

	if listed, listErr := storage.ObjectPacks(); listErr == nil {
		remaining = listed
		stats.PacksAfter = len(listed)
	}

	elapsed := slog.String("elapsed_time", stats.Duration.Truncate(time.Millisecond).String())

	if err != nil {
		stats.Result = MirrorCompactionFailed

		mirrorCompactionFailedAt.Store(path, time.Now())
		removeTempPacks(log, packDir)
		log.Warn("failed to compact bare mirror packfiles, keeping the existing packs",
			slog.String("path", path),
			slog.Group("packs",
				slog.Int("before", stats.PacksBefore),
				slog.Int("after", stats.PacksAfter),
			),
			slog.Group("size_bytes",
				slog.Int64("before", sizeBefore),
			),
			elapsed,
			slog.String("retry_delay", mirrorCompactRetryDelay.String()),
			slog.Any("error", err))

		return result.written
	}

	mirrorCompactionFailedAt.Delete(path)

	if !result.written {
		return false
	}

	stats.Result = MirrorCompactionCompacted

	size := []any{slog.Int64("before", sizeBefore)}

	// The size is informational, so a pack that cannot be measured only drops it from the log.
	if sizeAfter, sizeErr := packfilesSize(packDir, []plumbing.Hash{result.pack}); sizeErr == nil {
		size = append(size, slog.Int64("after", sizeAfter))
	}

	log.Info("compacted bare mirror packfiles",
		slog.String("path", path),
		slog.Group("packs",
			slog.Int("before", stats.PacksBefore),
			slog.Int("after", stats.PacksAfter),
		),
		slog.Group("size_bytes", size...),
		slog.Group("objects",
			slog.Int("total", result.objects),
			slog.Int("loose", result.looseObjects),
		),
		elapsed)

	return true
}

// packConsolidation describes the outcome of consolidatePacks.
type packConsolidation struct {
	// written reports whether the pack directory was modified.
	written bool
	// pack is the consolidated pack.
	pack plumbing.Hash
	// objects is the number of objects in the consolidated pack.
	objects int
	// looseObjects is the number of loose objects the consolidated pack replaces.
	looseObjects int
}

// consolidatePacks copies every object of the mirror into one new pack, verifies
// it, flushes it to disk and then deletes the packs and loose objects it
// replaces. The result reports whether the pack directory was modified, even
// when it also returns an error.
func consolidatePacks(storage *filesystem.Storage, packDir string, packs []plumbing.Hash) (packConsolidation, error) {
	var loose []plumbing.Hash

	if err := storage.ForEachObjectHash(func(h plumbing.Hash) error {
		loose = append(loose, h)
		return nil
	}); err != nil {
		return packConsolidation{}, fmt.Errorf("list loose objects: %w", err)
	}

	sources, err := loadPackSources(packDir, packs)
	if err != nil {
		return packConsolidation{}, fmt.Errorf("read packs: %w", err)
	}

	newPack, hashes, err := concatPacks(storage, packDir, sources, loose)
	if err != nil {
		return packConsolidation{}, err
	}

	if len(hashes) == 0 {
		return packConsolidation{}, nil
	}

	result := packConsolidation{
		written:      true,
		pack:         newPack,
		objects:      len(hashes),
		looseObjects: len(loose),
	}

	if err := verifyPack(packDir, newPack, hashes); err != nil {
		// The old packs still hold every object, so an incomplete new pack is
		// only dead weight.
		if !slices.Contains(packs, newPack) {
			if delErr := storage.DeleteOldObjectPackAndIndex(newPack, time.Time{}); delErr != nil {
				err = errors.Join(err, fmt.Errorf("remove unverified pack %s: %w", newPack, delErr))
			}
		}

		return result, err
	}

	// The renames that installed the new pack must reach the disk before the
	// removal of the packs it replaces, or a crash in between could leave the
	// mirror with neither.
	if err := syncPackDir(packDir); err != nil {
		return result, err
	}

	var errs []error

	for _, p := range packs {
		if p == newPack {
			continue
		}

		if err := storage.DeleteOldObjectPackAndIndex(p, time.Time{}); err != nil {
			errs = append(errs, fmt.Errorf("remove pack %s: %w", p, err))
			continue
		}

		removePackSidecars(packDir, p)
	}

	for _, h := range loose {
		if err := storage.DeleteLooseObject(h); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove loose object %s: %w", h, err))
		}
	}

	return result, errors.Join(errs...)
}

// syncPackDir flushes the pack directory's entries to disk; it can be overridden in tests.
var syncPackDir = syncDir

// syncDir flushes the entries of dir, such as files renamed into it, to disk.
func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- dir is a mirror's pack directory.
	if err != nil {
		return fmt.Errorf("open pack directory: %w", err)
	}

	err = d.Sync()
	if closeErr := d.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		return fmt.Errorf("sync pack directory: %w", err)
	}

	return nil
}

// verifyPack decodes the new pack's index from disk and checks it holds every object in hashes.
func verifyPack(packDir string, pack plumbing.Hash, hashes []plumbing.Hash) error {
	f, err := os.Open(filepath.Join(packDir, "pack-"+pack.String()+".idx")) // #nosec G304 -- path is built from the mirror directory and a pack checksum.
	if err != nil {
		return fmt.Errorf("open new pack index: %w", err)
	}

	defer func() { _ = f.Close() }()

	idx := idxfile.NewMemoryIndex()
	if err := idxfile.NewDecoder(f).Decode(idx); err != nil {
		return fmt.Errorf("decode new pack index: %w", err)
	}

	count, err := idx.Count()
	if err != nil {
		return fmt.Errorf("count new pack objects: %w", err)
	}

	if count != int64(len(hashes)) {
		return fmt.Errorf("new pack holds %d objects, want %d", count, len(hashes))
	}

	for _, h := range hashes {
		ok, err := idx.Contains(h)
		if err != nil {
			return fmt.Errorf("look up %s in new pack index: %w", h, err)
		}

		if !ok {
			return fmt.Errorf("new pack is missing object %s", h)
		}
	}

	return nil
}

// removePackSidecars deletes the files `git` keeps next to a pack and derives
// from it, which go-git does not remove along with the pack.
func removePackSidecars(packDir string, pack plumbing.Hash) {
	for _, ext := range []string{".rev", ".bitmap", ".mtimes"} {
		_ = os.Remove(filepath.Join(packDir, "pack-"+pack.String()+ext))
	}
}

// packfilesSize returns the combined size of the given packs' .pack files.
func packfilesSize(packDir string, packs []plumbing.Hash) (int64, error) {
	var total int64

	for _, p := range packs {
		info, err := os.Stat(filepath.Join(packDir, "pack-"+p.String()+".pack"))
		if err != nil {
			return 0, err
		}

		total += info.Size()
	}

	return total, nil
}

// removeTempPacks deletes temporary pack files left behind by an interrupted
// fetch or a failed compaction. Only writers holding the mirror's exclusive lock
// create them, so none can be in progress while the caller holds that lock.
func removeTempPacks(log *slog.Logger, packDir string) {
	entries, err := os.ReadDir(packDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), tempPackPrefix) {
			continue
		}

		if err := os.Remove(filepath.Join(packDir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Debug("failed to remove temporary pack file",
				slog.String("path", filepath.Join(packDir, entry.Name())),
				slog.Any("error", err))
		}
	}
}
