package git

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// MirrorCompactionMode selects how a mirror's packfiles are consolidated.
type MirrorCompactionMode string

const (
	// MirrorCompactionCopy concatenates the packs and keeps their compressed
	// entries, the way mirrors are compacted automatically after a fetch. It is
	// fast and needs little memory, but keeps every object's encoding, so the
	// new pack is about as large as the packs it replaces.
	MirrorCompactionCopy MirrorCompactionMode = "copy"
	// MirrorCompactionRepack re-encodes every object with a delta search across
	// the whole mirror. The new pack is considerably smaller, but the encoder
	// holds the objects in memory, several times the size of the packs, and
	// takes much longer.
	MirrorCompactionRepack MirrorCompactionMode = "repack"
)

// mirrorRepackWindow is the number of preceding objects a repack compares each
// object with when looking for a delta base. Wider windows shrink the pack
// further, but multiply the CPU time.
const mirrorRepackWindow = 10

// ErrInvalidMirrorCompactionMode indicates an unknown MirrorCompactionMode.
var ErrInvalidMirrorCompactionMode = errors.New("invalid mirror compaction mode")

// Valid reports whether m is a known compaction mode.
func (m MirrorCompactionMode) Valid() bool {
	return m == MirrorCompactionCopy || m == MirrorCompactionRepack
}

// MirrorCompactOptions configures CompactMirror.
type MirrorCompactOptions struct {
	// Repository is the mirrored repository in "<host>/<owner>/<repo>" form,
	// reported along with the mirror's statistics.
	Repository string
	Mode       MirrorCompactionMode
	// MaxSizeBytes skips a repack of a mirror whose packfiles are larger, since
	// the encoder needs several times their size in memory. Zero or less
	// disables the limit. Copy mode ignores it.
	MaxSizeBytes int64
}

// MirrorCompaction describes the outcome of a CompactMirror call.
type MirrorCompaction struct {
	MirrorPackStats
	// SizeBytesBefore is the combined size of the packfiles before the
	// compaction, or -1 if it could not be measured.
	SizeBytesBefore int64
	// Objects is the number of objects in the new pack.
	Objects int
	// LooseObjects is the number of loose objects the new pack replaced.
	LooseObjects int
}

// CompactMirror consolidates the packfiles and loose objects of the bare
// mirror at path into a single pack, in the given mode. Unlike the automatic
// compaction after a fetch, it runs regardless of the pack count.
//
// It skips the mirror instead of waiting when another operation holds its
// lock. Otherwise it holds the mirror's exclusive lock until it is done, so
// fetches and deploys of the repository wait for it. Once the context is
// cancelled, a repack stops and keeps the existing packs; a copy runs to
// completion, since it is bounded by disk throughput.
//
// The statistics are also reported to the MirrorPackObserver. A skipped mirror
// is not an error.
func CompactMirror(ctx context.Context, log *slog.Logger, path string, opts MirrorCompactOptions) (MirrorCompaction, error) {
	result := MirrorCompaction{
		Repository:      opts.Repository,
		Path:            path,
		PacksBefore:     -1,
		PacksAfter:      -1,
		SizeBytes:       -1,
		Mode:            opts.Mode,
		SizeBytesBefore: -1,
	}

	if !opts.Mode.Valid() {
		return result, fmt.Errorf("%w: %q", ErrInvalidMirrorCompactionMode, opts.Mode)
	}

	log = log.With(
		slog.String("repository", opts.Repository),
		slog.String("path", path),
		slog.String("mode", string(opts.Mode)))

	err := compactMirror(ctx, log, path, opts, &result)
	if result.Result == MirrorCompactionFailed {
		log.Warn("failed to compact bare mirror packfiles",
			slog.Group("packs",
				slog.Int("before", result.PacksBefore),
				slog.Int("after", result.PacksAfter),
			),
			slog.String("elapsed_time", result.Duration.Truncate(time.Millisecond).String()),
			slog.Any("error", err))
	}

	notifyMirrorPackObserver(result.MirrorPackStats)

	return result, err
}

func compactMirror(ctx context.Context, log *slog.Logger, path string, opts MirrorCompactOptions, result *MirrorCompaction) error {
	if err := ctx.Err(); err != nil {
		result.Result = MirrorCompactionCancelled
		return err
	}

	unlock, acquired, err := tryAcquireExclusiveMirrorLock(path)
	if err != nil {
		result.Result = MirrorCompactionFailed
		return fmt.Errorf("lock mirror: %w", err)
	}

	if !acquired {
		result.Result = MirrorCompactionSkippedBusy

		log.Info("skipped compaction of bare mirror in use")

		return nil
	}

	defer unlock()

	repo, err := git.PlainOpen(path)
	if err != nil {
		result.Result = MirrorCompactionFailed
		return fmt.Errorf("open mirror: %w", err)
	}

	storage, ok := repo.Storer.(*filesystem.Storage)
	if !ok {
		result.Result = MirrorCompactionFailed
		return errors.New("open mirror: no filesystem storage")
	}

	packDir := filepath.Join(path, "objects", "pack")

	// The exclusive lock rules out a fetch in progress, so any temporary pack
	// was left behind by an interrupted one and only skews the measurement.
	removeTempPacks(log, packDir)

	packs, err := storage.ObjectPacks()
	if err != nil {
		result.Result = MirrorCompactionFailed
		return fmt.Errorf("list packfiles: %w", err)
	}

	result.PacksBefore = len(packs)
	result.PacksAfter = len(packs)

	sizeBefore, err := packfilesSize(packDir, packs)
	if err != nil {
		result.Result = MirrorCompactionFailed
		return fmt.Errorf("measure packfiles: %w", err)
	}

	result.SizeBytesBefore = sizeBefore
	result.SizeBytes = sizeBefore

	write := copyPacks

	switch opts.Mode {
	case MirrorCompactionRepack:
		if opts.MaxSizeBytes > 0 && sizeBefore > opts.MaxSizeBytes {
			result.Result = MirrorCompactionSkippedSize

			log.Info("skipped repack of bare mirror above the size limit",
				slog.Group("size_bytes",
					slog.Int64("current", sizeBefore),
					slog.Int64("max", opts.MaxSizeBytes),
				))

			return nil
		}

		// Re-encoding a single pack can make it larger, for example one Git
		// wrote, so a repack that merges nothing only replaces it with a smaller one.
		var keepBelow int64

		if len(packs) == 1 {
			loose, err := hasLooseObjects(storage)
			if err != nil {
				result.Result = MirrorCompactionFailed
				return fmt.Errorf("list loose objects: %w", err)
			}

			if !loose {
				keepBelow = singlePackRepackLimit(sizeBefore)
			}
		}

		write = func(storage *filesystem.Storage, packDir string, _, _ []plumbing.Hash) (plumbing.Hash, []plumbing.Hash, error) {
			return repackObjects(ctx, storage, packDir, keepBelow)
		}
	case MirrorCompactionCopy:
		if len(packs) <= 1 {
			loose, err := hasLooseObjects(storage)
			if err != nil {
				result.Result = MirrorCompactionFailed
				return fmt.Errorf("list loose objects: %w", err)
			}

			if !loose {
				result.Result = MirrorCompactionSkippedSinglePack

				log.Info("skipped compaction of bare mirror with a single pack")

				return nil
			}
		}
	}

	start := time.Now()
	consolidation, err := consolidatePacks(storage, packDir, packs, write)
	result.Duration = time.Since(start)

	if listed, listErr := storage.ObjectPacks(); listErr == nil {
		result.PacksAfter = len(listed)
		result.SizeBytes = -1

		if size, sizeErr := packfilesSize(packDir, listed); sizeErr == nil {
			result.SizeBytes = size
		}
	}

	elapsed := slog.String("elapsed_time", result.Duration.Truncate(time.Millisecond).String())

	if notSmaller, ok := errors.AsType[*repackNotSmallerError](err); ok {
		result.Result = MirrorCompactionSkippedSinglePack

		log.Info("skipped compaction of bare mirror with a single pack, a repack would not make it smaller",
			slog.Group("size_bytes",
				slog.Int64("current", result.SizeBytesBefore),
				slog.Int64("repacked", notSmaller.size),
			),
			elapsed)

		return nil
	}

	if err != nil {
		removeTempPacks(log, packDir)

		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			result.Result = MirrorCompactionCancelled

			log.Info("cancelled compaction of bare mirror, keeping the existing packs", elapsed)

			return err
		}

		result.Result = MirrorCompactionFailed

		return err
	}

	// The mirror is compact now, so a failed automatic attempt need not hold off the next one.
	mirrorCompactionFailedAt.Delete(path)

	if !consolidation.written {
		result.Result = MirrorCompactionSkippedSinglePack
		return nil
	}

	result.Result = MirrorCompactionCompacted
	result.Objects = consolidation.objects
	result.LooseObjects = consolidation.looseObjects

	log.Info("compacted bare mirror packfiles",
		slog.Group("packs",
			slog.Int("before", result.PacksBefore),
			slog.Int("after", result.PacksAfter),
		),
		slog.Group("size_bytes",
			slog.Int64("before", result.SizeBytesBefore),
			slog.Int64("after", result.SizeBytes),
		),
		slog.Group("objects",
			slog.Int("total", result.Objects),
			slog.Int("loose", result.LooseObjects),
		),
		elapsed)

	return nil
}

// hasLooseObjects reports whether the mirror holds any loose object.
func hasLooseObjects(storage *filesystem.Storage) (bool, error) {
	found := false

	err := storage.ForEachObjectHash(func(plumbing.Hash) error {
		found = true
		return storer.ErrStop
	})
	if errors.Is(err, storer.ErrStop) {
		err = nil
	}

	return found, err
}

// singlePackRepackLimit returns the size a repack of a mirror's only pack, of
// size packSize, must stay below to replace it; tests override it.
var singlePackRepackLimit = func(packSize int64) int64 { return packSize }

// repackNotSmallerError reports that repackObjects discarded the new pack,
// since it was not below the requested size.
type repackNotSmallerError struct {
	size int64
}

func (e *repackNotSmallerError) Error() string {
	return fmt.Sprintf("repacked pack of %d bytes is not smaller than the existing pack", e.size)
}

// repackObjects re-encodes every object of the mirror, packed or loose, into
// one new pack in packDir and returns its checksum and the objects it holds.
// If keepBelow is positive and the new pack is not smaller, it discards the
// pack and returns a *repackNotSmallerError.
//
// The pack is written to a temporary file and indexed afterwards instead of
// through storage.PackfileWriter, whose Close leaves the temporary file and
// its descriptors behind when the encoder fails or is cancelled.
func repackObjects(ctx context.Context, storage *filesystem.Storage, packDir string, keepBelow int64) (plumbing.Hash, []plumbing.Hash, error) {
	// HashesWithPrefix lists loose objects as well, but only dedupes packed
	// objects against them; the same object may still be stored in several packs.
	all, err := storage.HashesWithPrefix(nil)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("list objects: %w", err)
	}

	seen := make(map[plumbing.Hash]struct{}, len(all))
	hashes := make([]plumbing.Hash, 0, len(all))

	for _, h := range all {
		if _, ok := seen[h]; ok {
			continue
		}

		seen[h] = struct{}{}
		hashes = append(hashes, h)
	}

	if len(hashes) == 0 {
		return plumbing.ZeroHash, nil, nil
	}

	tmpPack, err := os.CreateTemp(packDir, tempPackPrefix)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("create temporary pack: %w", err)
	}

	tmpPackPath := tmpPack.Name()

	defer func() {
		_ = tmpPack.Close()
		_ = os.Remove(tmpPackPath)
	}()

	w := bufio.NewWriterSize(contextWriter{ctx: ctx, w: tmpPack}, packCopyBufSize)

	checksum, err := packfile.NewEncoder(w, contextObjectStorer{Storage: storage, ctx: ctx}, false).
		Encode(hashes, mirrorRepackWindow)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("encode pack: %w", err)
	}

	if err := w.Flush(); err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("write pack: %w", err)
	}

	if keepBelow > 0 {
		info, err := tmpPack.Stat()
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("measure pack: %w", err)
		}

		if info.Size() >= keepBelow {
			return plumbing.ZeroHash, nil, &repackNotSmallerError{size: info.Size()}
		}
	}

	if err := tmpPack.Sync(); err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("sync pack: %w", err)
	}

	if _, err := tmpPack.Seek(0, io.SeekStart); err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("rewind pack: %w", err)
	}

	idx, err := indexPack(contextReadSeeker{ctx: ctx, r: tmpPack}, checksum)
	if err != nil {
		return plumbing.ZeroHash, nil, err
	}

	if err := tmpPack.Close(); err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("close pack: %w", err)
	}

	if err := installPack(packDir, tmpPackPath, checksum, idx); err != nil {
		return plumbing.ZeroHash, nil, err
	}

	return checksum, hashes, nil
}

// indexPack parses the pack r reads and builds its index. It fails unless the
// pack's trailing checksum is want.
func indexPack(r io.ReadSeeker, want plumbing.Hash) (*idxfile.Writer, error) {
	idx := new(idxfile.Writer)

	parser, err := packfile.NewParser(packfile.NewScanner(r), idx)
	if err != nil {
		return nil, fmt.Errorf("index pack: %w", err)
	}

	checksum, err := parser.Parse()
	if err != nil {
		return nil, fmt.Errorf("index pack: %w", err)
	}

	if checksum != want {
		return nil, fmt.Errorf("index pack: checksum %s, want %s", checksum, want)
	}

	return idx, nil
}

// contextWriter fails writes once ctx is done.
type contextWriter struct {
	ctx context.Context //nolint:containedctx // the encoder's io.Writer cannot take a context.
	w   io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}

	return w.w.Write(p)
}

// contextReadSeeker fails reads once ctx is done.
type contextReadSeeker struct {
	ctx context.Context //nolint:containedctx // the parser's io.Reader cannot take a context.
	r   io.ReadSeeker
}

func (r contextReadSeeker) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}

	return r.r.Read(p)
}

func (r contextReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return r.r.Seek(offset, whence)
}

// contextObjectStorer fails object reads once ctx is done. The encoder reads
// every object during its delta search, before it writes anything, so a
// failing writer alone would only stop it after the most expensive part.
type contextObjectStorer struct {
	*filesystem.Storage

	ctx context.Context //nolint:containedctx // the encoder's storer cannot take a context.
}

func (s contextObjectStorer) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}

	return s.Storage.EncodedObject(t, h)
}

// DeltaObject keeps the storage's storer.DeltaObjectStorer implementation
// visible to the encoder, which reuses existing deltas through it.
func (s contextObjectStorer) DeltaObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}

	return s.Storage.DeltaObject(t, h)
}
