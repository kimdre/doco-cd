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
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

const (
	// mirrorCompactPackThreshold is the packfile count above which a bare mirror
	// is consolidated into a single pack. Every fetch that brings new objects
	// writes another pack, and every freshly opened handle loads all pack indexes
	// on its first object read, so an unbounded pack count slows down every read.
	mirrorCompactPackThreshold = 32

	// mirrorCompactMaxPackBytes skips compaction of mirrors whose packs exceed
	// this total size: the encoder keeps per-object metadata in memory, so very
	// large mirrors are left to an operator-run `git gc` instead.
	mirrorCompactMaxPackBytes int64 = 256 << 20

	// mirrorCompactPackWindow matches git's default pack.window. A non-zero
	// window also lets the encoder reuse the deltas already stored in the packs.
	mirrorCompactPackWindow uint = 10

	// mirrorCompactRetryDelay keeps a mirror whose compaction failed from
	// repeating the attempt, and its cost, on every poll.
	mirrorCompactRetryDelay = time.Hour

	tempPackPrefix = "tmp_pack_"
)

// Results reported in MirrorPackStats.Result when a compaction was attempted.
const (
	MirrorCompactionCompacted   = "compacted"
	MirrorCompactionSkippedSize = "skipped_size"
	MirrorCompactionFailed      = "failed"
)

// MirrorPackStats describes a bare mirror's packfiles after a fetch and the
// compaction it triggered, if any.
type MirrorPackStats struct {
	// Repository is the mirrored repository in "<host>/<owner>/<repo>" form.
	Repository  string
	PacksBefore int
	PacksAfter  int
	// Result is empty when the pack count stayed within the compaction threshold.
	Result   string
	Duration time.Duration
}

// MirrorPackObserver receives MirrorPackStats after every bare mirror fetch.
type MirrorPackObserver func(MirrorPackStats)

var (
	mirrorPackObserver atomic.Pointer[MirrorPackObserver]

	// mirrorCompactionSizeWarned remembers the mirrors already reported as too
	// large to compact, so polling does not repeat the warning on every fetch.
	mirrorCompactionSizeWarned sync.Map

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

	stats := MirrorPackStats{
		Repository:  repository,
		PacksBefore: len(packs),
		PacksAfter:  len(packs),
	}

	defer func() { notifyMirrorPackObserver(stats) }()

	if len(packs) <= mirrorCompactPackThreshold {
		return false
	}

	if failedAt, ok := mirrorCompactionFailedAt.Load(path); ok && time.Since(failedAt.(time.Time)) < mirrorCompactRetryDelay {
		return false
	}

	packDir := filepath.Join(path, "objects", "pack")

	size, err := packfilesSize(packDir, packs)
	if err != nil {
		stats.Result = MirrorCompactionFailed

		mirrorCompactionFailedAt.Store(path, time.Now())
		log.Warn("failed to measure bare mirror packfiles, skipping compaction",
			slog.String("path", path),
			slog.Any("error", err))

		return false
	}

	if size > mirrorCompactMaxPackBytes {
		stats.Result = MirrorCompactionSkippedSize

		if _, warned := mirrorCompactionSizeWarned.LoadOrStore(path, struct{}{}); !warned {
			log.Warn("bare mirror packfiles are too large to compact automatically, run `git gc` on the mirror to consolidate them",
				slog.String("path", path),
				slog.Int("packs", len(packs)),
				slog.Int64("size_bytes", size),
				slog.Int64("limit_bytes", mirrorCompactMaxPackBytes))
		}

		return false
	}

	removeTempPacks(log, packDir)

	start := time.Now()
	written, err := consolidatePacks(storage, packDir, packs)
	stats.Duration = time.Since(start)

	if remaining, listErr := storage.ObjectPacks(); listErr == nil {
		stats.PacksAfter = len(remaining)
	}

	if err != nil {
		stats.Result = MirrorCompactionFailed

		mirrorCompactionFailedAt.Store(path, time.Now())
		removeTempPacks(log, packDir)
		log.Warn("failed to compact bare mirror packfiles, keeping the existing packs",
			slog.String("path", path),
			slog.Int("packs", len(packs)),
			slog.Duration("retry_delay", mirrorCompactRetryDelay),
			slog.Any("error", err))

		return written
	}

	mirrorCompactionFailedAt.Delete(path)

	if !written {
		return false
	}

	stats.Result = MirrorCompactionCompacted

	log.Info("compacted bare mirror packfiles",
		slog.String("path", path),
		slog.Int("packs_before", stats.PacksBefore),
		slog.Int("packs_after", stats.PacksAfter),
		slog.Int64("size_bytes_before", size),
		slog.Duration("duration", stats.Duration))

	return true
}

// consolidatePacks writes every object of the mirror into one new pack, verifies
// it and then deletes the packs and loose objects it replaces. It reports
// whether the pack directory was modified, even when it also returns an error.
func consolidatePacks(storage *filesystem.Storage, packDir string, packs []plumbing.Hash) (bool, error) {
	var loose []plumbing.Hash

	if err := storage.ForEachObjectHash(func(h plumbing.Hash) error {
		loose = append(loose, h)
		return nil
	}); err != nil {
		return false, fmt.Errorf("list loose objects: %w", err)
	}

	// HashesWithPrefix only dedupes packed objects against loose ones; the same
	// object may still be stored in several packs.
	all, err := storage.HashesWithPrefix(nil)
	if err != nil {
		return false, fmt.Errorf("list packed objects: %w", err)
	}

	unique := make(map[plumbing.Hash]struct{}, len(all))
	hashes := make([]plumbing.Hash, 0, len(all))

	for _, h := range all {
		if _, ok := unique[h]; ok {
			continue
		}

		unique[h] = struct{}{}
		hashes = append(hashes, h)
	}

	if len(hashes) == 0 {
		return false, nil
	}

	newPack, err := writePack(storage, hashes)
	if err != nil {
		return false, err
	}

	if err := verifyPack(packDir, newPack, hashes); err != nil {
		// The old packs still hold every object, so an incomplete new pack is
		// only dead weight.
		if !slices.Contains(packs, newPack) {
			if delErr := storage.DeleteOldObjectPackAndIndex(newPack, time.Time{}); delErr != nil {
				err = errors.Join(err, fmt.Errorf("remove unverified pack %s: %w", newPack, delErr))
			}
		}

		return true, err
	}

	var errs []error

	for _, p := range packs {
		if p == newPack {
			continue
		}

		if err := storage.DeleteOldObjectPackAndIndex(p, time.Time{}); err != nil {
			errs = append(errs, fmt.Errorf("remove pack %s: %w", p, err))
		}
	}

	for _, h := range loose {
		if err := storage.DeleteLooseObject(h); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove loose object %s: %w", h, err))
		}
	}

	return true, errors.Join(errs...)
}

// writePack encodes hashes into a new pack in the mirror's object directory and
// returns the new pack's checksum, which is also its file name.
func writePack(storage *filesystem.Storage, hashes []plumbing.Hash) (plumbing.Hash, error) {
	w, err := storage.PackfileWriter()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("create pack writer: %w", err)
	}

	newPack, encodeErr := packfile.NewEncoder(w, storage, false).Encode(hashes, mirrorCompactPackWindow)

	// Close parses the written pack, then writes its index and renames it into
	// place; it must run even after a failed encode to stop the indexer.
	closeErr := w.Close()

	if encodeErr != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode pack: %w", encodeErr)
	}

	if closeErr != nil {
		return plumbing.ZeroHash, fmt.Errorf("finalize pack: %w", closeErr)
	}

	return newPack, nil
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
