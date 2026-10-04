package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// setupMirrorWithLargeFile returns a bare mirror holding packs packfiles, one
// per fetched commit, each of which rewrites a large file with a small change.
// Every pack holds the file in full, which only a repack can turn into deltas.
func setupMirrorWithLargeFile(t *testing.T, packs int) (mirrorPath string, commits []plumbing.Hash) {
	t.Helper()

	originPath, mirrorPath, mainHash := setupMirrorWithPack(t)
	commits = append(commits, mainHash)

	// Hex-encoded hash chain: deterministic and barely compressible.
	var b strings.Builder

	sum := sha256.Sum256([]byte("seed"))
	for range 512 {
		b.WriteString(hex.EncodeToString(sum[:]))
		sum = sha256.Sum256(sum[:])
	}

	content := b.String()

	for i := 1; i < packs; i++ {
		commits = append(commits, fetchNewCommitIntoMirror(t, originPath, mirrorPath,
			content+"revision "+strconv.Itoa(i)+"\n", "commit "+strconv.Itoa(i)))
	}

	if got := countPacks(t, mirrorPath); got != packs {
		t.Fatalf("mirror holds %d packs, want %d", got, packs)
	}

	return mirrorPath, commits
}

// assertNoTempPacks fails the test if the mirror's pack directory holds temporary files.
func assertNoTempPacks(t *testing.T, mirrorPath string) {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(mirrorPath, "objects", "pack", tempPackPrefix+"*"))
	if err != nil {
		t.Fatalf("glob temporary packs: %v", err)
	}

	if len(matches) != 0 {
		t.Fatalf("temporary pack files left behind: %v", matches)
	}
}

// assertNoLooseObjects fails the test if the mirror holds loose objects.
func assertNoLooseObjects(t *testing.T, mirrorPath string) {
	t.Helper()

	if err := openMirror(t, mirrorPath).Storer.(*filesystem.Storage).ForEachObjectHash(func(h plumbing.Hash) error {
		return errors.New("loose object left behind: " + h.String())
	}); err != nil {
		t.Fatal(err)
	}
}

// storeLooseBlob writes content into the mirror as a loose object.
func storeLooseBlob(t *testing.T, mirrorPath, content string) plumbing.Hash {
	t.Helper()

	repo := openMirror(t, mirrorPath)

	obj := repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)

	w, err := obj.Writer()
	if err != nil {
		t.Fatalf("loose object writer: %v", err)
	}

	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatalf("write loose object: %v", err)
	}

	_ = w.Close()

	h, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("store loose object: %v", err)
	}

	return h
}

// cancelAfterContext reports cancellation from its (n+1)th Err call on, which
// cancels a compaction at a deterministic point after it started.
type cancelAfterContext struct {
	context.Context //nolint:containedctx // wraps the parent to override Err.

	n     int64
	calls atomic.Int64
}

func (c *cancelAfterContext) Err() error {
	if c.calls.Add(1) > c.n {
		return context.Canceled
	}

	return nil
}

// Not parallel: the pack observer is process-global.
func TestCompactMirror_RepackShrinksMirror(t *testing.T) {
	stats := captureMirrorPackStats(t)

	mirrorPath, commits := setupMirrorWithLargeFile(t, 5)
	looseHash := storeLooseBlob(t, mirrorPath, "loose object\n")
	sizeBefore := packsSize(t, mirrorPath)

	log, logRecords := captureLogRecords(t)

	result, err := CompactMirror(t.Context(), log, mirrorPath, MirrorCompactOptions{
		Repository:   "example.com/owner/repo",
		Mode:         MirrorCompactionRepack,
		MaxSizeBytes: sizeBefore,
	})
	if err != nil {
		t.Fatalf("CompactMirror() error = %v", err)
	}

	if result.Result != MirrorCompactionCompacted || result.PacksBefore != 5 || result.PacksAfter != 1 {
		t.Fatalf("CompactMirror() = %+v, want 5 packs compacted into 1", result)
	}

	if result.SizeBytesBefore != sizeBefore || result.SizeBytes != packsSize(t, mirrorPath) {
		t.Fatalf("CompactMirror() sizes = %d -> %d, want %d -> %d",
			result.SizeBytesBefore, result.SizeBytes, sizeBefore, packsSize(t, mirrorPath))
	}

	// Each pack holds the large file in full; the repack keeps it once and deltas against it.
	if result.SizeBytes*2 > sizeBefore {
		t.Fatalf("repacked size = %d, want less than half of %d", result.SizeBytes, sizeBefore)
	}

	if result.LooseObjects != 1 || result.Objects <= 1 {
		t.Fatalf("CompactMirror() objects = %d total, %d loose, want several and 1", result.Objects, result.LooseObjects)
	}

	if countPacks(t, mirrorPath) != 1 {
		t.Fatalf("mirror holds %d packs, want 1", countPacks(t, mirrorPath))
	}

	assertNoTempPacks(t, mirrorPath)
	assertNoLooseObjects(t, mirrorPath)

	fresh := openMirror(t, mirrorPath)
	if err := readCommits(fresh, commits); err != nil {
		t.Fatalf("read commits after repack: %v", err)
	}

	if _, err := fresh.BlobObject(looseHash); err != nil {
		t.Fatalf("formerly loose object missing from the new pack: %v", err)
	}

	gitFsck(t, mirrorPath)

	got := stats()
	if len(got) != 1 || got[0] != result.MirrorPackStats {
		t.Fatalf("observer stats = %+v, want [%+v]", got, result.MirrorPackStats)
	}

	if got[0].Mode != MirrorCompactionRepack || got[0].Duration <= 0 {
		t.Fatalf("observer stats = %+v, want mode %q and a duration", got[0], MirrorCompactionRepack)
	}

	records := logRecords()
	if len(records) != 1 || records[0]["msg"] != "compacted bare mirror packfiles" {
		t.Fatalf("log records = %v, want one compaction record", records)
	}

	for key, want := range map[string]any{
		"repository":    "example.com/owner/repo",
		"mode":          "repack",
		"packs.before":  float64(5),
		"packs.after":   float64(1),
		"objects.loose": float64(1),
	} {
		if got := logField(records[0], key); got != want {
			t.Errorf("log record %s = %v, want %v", key, got, want)
		}
	}

	assertElapsedTime(t, records[0])
}

func TestCompactMirror_CopyConsolidatesPacksAndLooseObjects(t *testing.T) {
	t.Parallel()

	_, mirrorPath, commits := setupMirrorWithPacks(t, 3)
	looseHash := storeLooseBlob(t, mirrorPath, "loose object\n")

	// The size limit only guards repacks.
	result, err := CompactMirror(t.Context(), discardLog, mirrorPath, MirrorCompactOptions{
		Mode:         MirrorCompactionCopy,
		MaxSizeBytes: 1,
	})
	if err != nil {
		t.Fatalf("CompactMirror() error = %v", err)
	}

	if result.Result != MirrorCompactionCompacted || result.Mode != MirrorCompactionCopy ||
		result.PacksBefore != 3 || result.PacksAfter != 1 || result.LooseObjects != 1 {
		t.Fatalf("CompactMirror() = %+v, want 3 packs and 1 loose object copied into 1 pack", result)
	}

	assertNoTempPacks(t, mirrorPath)
	assertNoLooseObjects(t, mirrorPath)

	fresh := openMirror(t, mirrorPath)
	if err := readCommits(fresh, commits); err != nil {
		t.Fatalf("read commits after copy: %v", err)
	}

	if _, err := fresh.BlobObject(looseHash); err != nil {
		t.Fatalf("formerly loose object missing from the new pack: %v", err)
	}

	gitFsck(t, mirrorPath)
}

func TestCompactMirror_SinglePack(t *testing.T) {
	t.Parallel()

	_, mirrorPath, _ := setupMirrorWithPack(t)

	// A copy would only rewrite the pack unchanged.
	result, err := CompactMirror(t.Context(), discardLog, mirrorPath, MirrorCompactOptions{Mode: MirrorCompactionCopy})
	if err != nil || result.Result != MirrorCompactionSkippedSinglePack {
		t.Fatalf("CompactMirror(copy) = %+v, %v, want %q", result, err, MirrorCompactionSkippedSinglePack)
	}

	if result.PacksBefore != 1 || result.PacksAfter != 1 || result.SizeBytes != packsSize(t, mirrorPath) {
		t.Fatalf("CompactMirror(copy) = %+v, want the single pack and its size", result)
	}
}

func TestCompactMirror_RepacksCopiedPack(t *testing.T) {
	t.Parallel()

	mirrorPath, commits := setupMirrorWithLargeFile(t, 5)

	// A copy keeps the large file in full once per former pack.
	result, err := CompactMirror(t.Context(), discardLog, mirrorPath, MirrorCompactOptions{Mode: MirrorCompactionCopy})
	if err != nil || result.Result != MirrorCompactionCompacted || result.PacksAfter != 1 {
		t.Fatalf("CompactMirror(copy) = %+v, %v, want 5 packs copied into 1", result, err)
	}

	copied := packsSize(t, mirrorPath)

	result, err = CompactMirror(t.Context(), discardLog, mirrorPath, MirrorCompactOptions{Mode: MirrorCompactionRepack})
	if err != nil || result.Result != MirrorCompactionCompacted || result.PacksBefore != 1 || result.PacksAfter != 1 {
		t.Fatalf("CompactMirror(repack) = %+v, %v, want the single pack re-encoded", result, err)
	}

	if result.SizeBytesBefore != copied || result.SizeBytes*2 > copied {
		t.Fatalf("CompactMirror(repack) sizes = %d -> %d, want %d -> less than half", result.SizeBytesBefore, result.SizeBytes, copied)
	}

	assertNoTempPacks(t, mirrorPath)

	if err := readCommits(openMirror(t, mirrorPath), commits); err != nil {
		t.Fatalf("read commits after repack: %v", err)
	}

	gitFsck(t, mirrorPath)
}

// Not parallel: overrides singlePackRepackLimit.
func TestCompactMirror_KeepsSinglePackUnlessRepackIsSmaller(t *testing.T) {
	singlePackRepackLimit = func(int64) int64 { return 1 }

	t.Cleanup(func() { singlePackRepackLimit = func(packSize int64) int64 { return packSize } })

	_, mirrorPath, mainHash := setupMirrorWithPack(t)

	packFiles := func() []string {
		t.Helper()

		matches, err := filepath.Glob(filepath.Join(mirrorPath, "objects", "pack", "*"))
		if err != nil {
			t.Fatalf("glob pack files: %v", err)
		}

		return matches
	}

	before := packFiles()
	log, logRecords := captureLogRecords(t)

	result, err := CompactMirror(t.Context(), log, mirrorPath, MirrorCompactOptions{Mode: MirrorCompactionRepack})
	if err != nil || result.Result != MirrorCompactionSkippedSinglePack {
		t.Fatalf("CompactMirror(repack) = %+v, %v, want %q", result, err, MirrorCompactionSkippedSinglePack)
	}

	if result.PacksAfter != 1 || result.SizeBytes != result.SizeBytesBefore || result.Duration <= 0 {
		t.Fatalf("CompactMirror(repack) = %+v, want the single pack kept and a duration", result)
	}

	if after := packFiles(); !slices.Equal(after, before) {
		t.Fatalf("pack files = %v, want them unchanged: %v", after, before)
	}

	records := logRecords()
	if len(records) != 1 || !strings.HasPrefix(records[0]["msg"].(string), "skipped compaction of bare mirror with a single pack") {
		t.Fatalf("log records = %v, want one skip record", records)
	}

	if got := logField(records[0], "size_bytes.current"); got != float64(result.SizeBytesBefore) {
		t.Errorf("log record size_bytes.current = %v, want %d", got, result.SizeBytesBefore)
	}

	if got, ok := logField(records[0], "size_bytes.repacked").(float64); !ok || got <= 0 {
		t.Errorf("log record size_bytes.repacked = %v, want the repacked size", logField(records[0], "size_bytes.repacked"))
	}

	assertElapsedTime(t, records[0])

	// A loose object leaves something to merge, so the repack replaces the pack regardless of its size.
	storeLooseBlob(t, mirrorPath, "loose object\n")

	result, err = CompactMirror(t.Context(), discardLog, mirrorPath, MirrorCompactOptions{Mode: MirrorCompactionRepack})
	if err != nil || result.Result != MirrorCompactionCompacted || result.LooseObjects != 1 {
		t.Fatalf("CompactMirror(repack) = %+v, %v, want the pack and loose object repacked", result, err)
	}

	assertNoTempPacks(t, mirrorPath)
	assertNoLooseObjects(t, mirrorPath)

	if err := readCommits(openMirror(t, mirrorPath), []plumbing.Hash{mainHash}); err != nil {
		t.Fatalf("read commits after repack: %v", err)
	}

	gitFsck(t, mirrorPath)
}

func TestCompactMirror_SkipsRepackAboveSizeLimit(t *testing.T) {
	t.Parallel()

	_, mirrorPath, _ := setupMirrorWithPacks(t, 3)
	size := packsSize(t, mirrorPath)

	result, err := CompactMirror(t.Context(), discardLog, mirrorPath, MirrorCompactOptions{
		Mode:         MirrorCompactionRepack,
		MaxSizeBytes: size - 1,
	})
	if err != nil || result.Result != MirrorCompactionSkippedSize {
		t.Fatalf("CompactMirror() = %+v, %v, want %q", result, err, MirrorCompactionSkippedSize)
	}

	if result.PacksBefore != 3 || result.PacksAfter != 3 || result.SizeBytesBefore != size || result.SizeBytes != size {
		t.Fatalf("CompactMirror() = %+v, want 3 packs of %d bytes left alone", result, size)
	}

	if countPacks(t, mirrorPath) != 3 {
		t.Fatalf("mirror holds %d packs, want 3", countPacks(t, mirrorPath))
	}
}

func TestCompactMirror_SkipsMirrorInUse(t *testing.T) {
	t.Parallel()

	_, mirrorPath, _ := setupMirrorWithPacks(t, 3)

	unlock := AcquireSharedMirrorLock(mirrorPath)
	defer unlock()

	result, err := CompactMirror(t.Context(), discardLog, mirrorPath, MirrorCompactOptions{Mode: MirrorCompactionRepack})
	if err != nil || result.Result != MirrorCompactionSkippedBusy {
		t.Fatalf("CompactMirror() = %+v, %v, want %q", result, err, MirrorCompactionSkippedBusy)
	}

	// Nothing is read from a mirror in use.
	if result.PacksBefore != -1 || result.PacksAfter != -1 || result.SizeBytes != -1 || result.SizeBytesBefore != -1 {
		t.Fatalf("CompactMirror() = %+v, want unknown pack counts and sizes", result)
	}

	if countPacks(t, mirrorPath) != 3 {
		t.Fatalf("mirror holds %d packs, want 3", countPacks(t, mirrorPath))
	}
}

func TestCompactMirror_CancelledRepackKeepsPacks(t *testing.T) {
	t.Parallel()

	for _, n := range []int64{0, 1, 4} {
		t.Run(strconv.FormatInt(n, 10), func(t *testing.T) {
			t.Parallel()

			_, mirrorPath, commits := setupMirrorWithPacks(t, 3)
			ctx := &cancelAfterContext{Context: t.Context(), n: n}

			result, err := CompactMirror(ctx, discardLog, mirrorPath, MirrorCompactOptions{Mode: MirrorCompactionRepack})
			if !errors.Is(err, context.Canceled) || result.Result != MirrorCompactionCancelled {
				t.Fatalf("CompactMirror() = %+v, %v, want %q with context.Canceled", result, err, MirrorCompactionCancelled)
			}

			if countPacks(t, mirrorPath) != 3 {
				t.Fatalf("mirror holds %d packs, want 3", countPacks(t, mirrorPath))
			}

			assertNoTempPacks(t, mirrorPath)

			if err := readCommits(openMirror(t, mirrorPath), commits); err != nil {
				t.Fatalf("read commits after cancelled repack: %v", err)
			}

			// The lock was released.
			unlock, acquired, err := tryAcquireExclusiveMirrorLock(mirrorPath)
			if err != nil || !acquired {
				t.Fatalf("tryAcquireExclusiveMirrorLock() = %v, %v, want acquired", acquired, err)
			}

			unlock()
		})
	}
}

func TestCompactMirror_RejectsUnknownMode(t *testing.T) {
	t.Parallel()

	_, mirrorPath, _ := setupMirrorWithPacks(t, 2)

	_, err := CompactMirror(t.Context(), discardLog, mirrorPath, MirrorCompactOptions{Mode: "gc"})
	if !errors.Is(err, ErrInvalidMirrorCompactionMode) {
		t.Fatalf("CompactMirror() error = %v, want ErrInvalidMirrorCompactionMode", err)
	}
}

func TestCompactMirror_FailsOnMissingMirror(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "mirror")

	result, err := CompactMirror(t.Context(), discardLog, path, MirrorCompactOptions{Mode: MirrorCompactionCopy})
	if err == nil || result.Result != MirrorCompactionFailed {
		t.Fatalf("CompactMirror() = %+v, %v, want %q with an error", result, err, MirrorCompactionFailed)
	}

	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("CompactMirror() created %s: %v", path, statErr)
	}
}

func TestCompactMirror_RepackOppositeStoredDeltas(t *testing.T) {
	t.Parallel()

	path, storage, packDir := newBareTestRepo(t)
	a := newTestBlob(strings.Repeat("base content\n", 256) + "version a\n")
	b := newTestBlob(strings.Repeat("base content\n", 256) + "version b\n")

	// Both packs are valid, but independently picking their stored deltas
	// could select A -> B and B -> A and recurse forever in go-git's encoder.
	writeTestPack(t, packDir, time.Now(), fullEntry(a), refDeltaEntry(b, a))
	writeTestPack(t, packDir, time.Now(), fullEntry(b), refDeltaEntry(a, b))

	objects, hashes, err := newRepackObjectStorer(t.Context(), storage)
	if err != nil || len(hashes) != 2 {
		t.Fatalf("newRepackObjectStorer() = %v, %v, want 2 objects", hashes, err)
	}

	for _, blob := range []testBlob{a, b} {
		obj, err := objects.DeltaObject(plumbing.AnyObject, blob.hash)
		if err != nil {
			t.Fatal(err)
		}

		if _, ok := obj.(plumbing.DeltaObject); ok {
			t.Fatalf("DeltaObject(%s) reused a delta of an object stored in both packs", blob.hash)
		}
	}

	result, err := CompactMirror(t.Context(), discardLog, path, MirrorCompactOptions{Mode: MirrorCompactionRepack})
	if err != nil || result.Result != MirrorCompactionCompacted || result.Objects != 2 || result.PacksAfter != 1 {
		t.Fatalf("CompactMirror() = %+v, %v, want 2 objects in 1 pack", result, err)
	}

	for _, blob := range []testBlob{a, b} {
		if got := readBlob(t, path, blob.hash); !bytes.Equal(got, blob.content) {
			t.Fatalf("blob %s changed after repack", blob.hash)
		}
	}

	assertNoTempPacks(t, path)
	gitFsck(t, path)
}

func TestRepackObjectStorer_ReusesDeltasOfObjectsStoredOnce(t *testing.T) {
	t.Parallel()

	path, storage, packDir := newBareTestRepo(t)
	a := newTestBlob(strings.Repeat("base content\n", 256) + "version a\n")
	b := newTestBlob(strings.Repeat("base content\n", 256) + "version b\n")
	c := newTestBlob(strings.Repeat("base content\n", 256) + "version c\n")

	// B is stored in both packs, A and C only in one.
	writeTestPack(t, packDir, time.Now(), fullEntry(a), refDeltaEntry(b, a))
	writeTestPack(t, packDir, time.Now(), fullEntry(b), refDeltaEntry(c, b))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	objects, hashes, err := newRepackObjectStorer(ctx, storage)
	if err != nil || len(hashes) != 3 {
		t.Fatalf("newRepackObjectStorer() = %v, %v, want 3 objects", hashes, err)
	}

	obj, err := objects.DeltaObject(plumbing.AnyObject, c.hash)
	if err != nil {
		t.Fatal(err)
	}

	delta, ok := obj.(plumbing.DeltaObject)
	if !ok || delta.BaseHash() != b.hash {
		t.Fatalf("DeltaObject(C) = %T, want its stored delta against B", obj)
	}

	for _, blob := range []testBlob{a, b} {
		obj, err := objects.DeltaObject(plumbing.AnyObject, blob.hash)
		if err != nil {
			t.Fatal(err)
		}

		if _, ok := obj.(plumbing.DeltaObject); ok || obj.Size() != int64(len(blob.content)) {
			t.Fatalf("DeltaObject(%s) = %T of %d bytes, want the full object", blob.hash, obj, obj.Size())
		}
	}

	cancel()

	if _, err := delta.Reader(); !errors.Is(err, context.Canceled) {
		t.Fatalf("stored delta Reader() error = %v, want context.Canceled", err)
	}

	result, err := CompactMirror(t.Context(), discardLog, path, MirrorCompactOptions{Mode: MirrorCompactionRepack})
	if err != nil || result.Result != MirrorCompactionCompacted || result.Objects != 3 || result.PacksAfter != 1 {
		t.Fatalf("CompactMirror() = %+v, %v, want 3 objects in 1 pack", result, err)
	}

	for _, blob := range []testBlob{a, b, c} {
		if got := readBlob(t, path, blob.hash); !bytes.Equal(got, blob.content) {
			t.Fatalf("blob %s changed after repack", blob.hash)
		}
	}

	assertNoTempPacks(t, path)
	gitFsck(t, path)
}

func TestContextObjectStorer_CancelsLoadedObjectReaders(t *testing.T) {
	t.Parallel()

	path, storage, _ := newBareTestRepo(t)
	hash := storeLooseBlob(t, path, "object content\n")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	objects := contextObjectStorer{EncodedObjectStorer: storage, ctx: ctx}

	obj, err := objects.EncodedObject(plumbing.BlobObject, hash)
	if err != nil {
		t.Fatal(err)
	}

	if obj.Hash() != hash || obj.Type() != plumbing.BlobObject || obj.Size() != int64(len("object content\n")) {
		t.Fatalf("wrapped object lost its metadata: %s, %s, %d", obj.Hash(), obj.Type(), obj.Size())
	}

	r, err := obj.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	cancel()

	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("loaded object Read() error = %v, want context.Canceled", err)
	}

	if _, err := obj.Reader(); !errors.Is(err, context.Canceled) {
		t.Fatalf("loaded object Reader() error = %v, want context.Canceled", err)
	}
}

func TestContextEncodedObject_KeepsWriterTo(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	mem := &plumbing.MemoryObject{}
	mem.SetType(plumbing.BlobObject)

	if _, err := mem.Write([]byte("object content\n")); err != nil {
		t.Fatal(err)
	}

	obj := contextEncodedObject{EncodedObject: mem, ctx: ctx}

	r, err := obj.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	wt, ok := r.(io.WriterTo)
	if !ok {
		t.Fatalf("reader %T of an in-memory object lost io.WriterTo", r)
	}

	var buf bytes.Buffer
	if _, err := wt.WriteTo(&buf); err != nil || buf.String() != "object content\n" {
		t.Fatalf("WriteTo() = %q, %v", buf.String(), err)
	}

	r2, err := obj.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Close() }()

	cancel()

	if _, err := r2.(io.WriterTo).WriteTo(io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteTo() error = %v, want context.Canceled", err)
	}
}

type cancelOnObjectReadStorer struct {
	storer.EncodedObjectStorer

	cancel context.CancelFunc
}

func (s cancelOnObjectReadStorer) EncodedObject(typ plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) {
	obj, err := s.EncodedObjectStorer.EncodedObject(typ, hash)
	if err != nil {
		return nil, err
	}

	return cancelOnObjectRead{EncodedObject: obj, cancel: s.cancel}, nil
}

type cancelOnObjectRead struct {
	plumbing.EncodedObject

	cancel context.CancelFunc
}

func (o cancelOnObjectRead) Reader() (io.ReadCloser, error) {
	o.cancel()
	return o.EncodedObject.Reader()
}

func TestContextObjectStorer_CancelsDuringDeltaSearch(t *testing.T) {
	t.Parallel()

	path, storage, _ := newBareTestRepo(t)
	hashes := []plumbing.Hash{
		storeLooseBlob(t, path, strings.Repeat("common content\n", 256)+"version a\n"),
		storeLooseBlob(t, path, strings.Repeat("common content\n", 256)+"version b\n"),
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Initial lookups succeed. Opening the first content reader cancels the
	// context inside the delta search, before any pack write can notice it.
	objects := contextObjectStorer{
		EncodedObjectStorer: cancelOnObjectReadStorer{EncodedObjectStorer: storage, cancel: cancel},
		ctx:                 ctx,
	}

	_, err := packfile.NewEncoder(io.Discard, objects, false).Encode(hashes, mirrorRepackWindow)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Encode() error = %v, want cancellation during delta search", err)
	}
}

// Not parallel: overrides the process-global pack and lock observers.
func TestCompactMirror_ReportsBeforeUnlock(t *testing.T) {
	originPath, mirrorPath, _ := setupMirrorWithPacks(t, 3)
	stats := captureMirrorPackStats(t)

	var fetched atomic.Bool

	SetMirrorLockObserver(func(mode string, _, _ time.Duration) {
		if mode != MirrorLockExclusive || fetched.Swap(true) {
			return
		}

		// This callback runs after the underlying unlock, deterministically
		// simulating a waiting fetch that reports before CompactMirror returns.
		fetchNewCommitIntoMirror(t, originPath, mirrorPath, "new content\n", "new commit")

		unlock := AcquireExclusiveMirrorLock(mirrorPath)
		reportMirrorPacksLocked(openMirror(t, mirrorPath), mirrorPath, "")
		unlock()
	})
	t.Cleanup(func() { SetMirrorLockObserver(nil) })

	result, err := CompactMirror(t.Context(), discardLog, mirrorPath, MirrorCompactOptions{Mode: MirrorCompactionCopy})
	if err != nil || result.Result != MirrorCompactionCompacted {
		t.Fatalf("CompactMirror() = %+v, %v, want compacted", result, err)
	}

	got := stats()
	if len(got) != 2 || got[0] != result.MirrorPackStats || got[1].PacksAfter != 2 || got[1].SizeBytes != packsSize(t, mirrorPath) {
		t.Fatalf("observer stats = %+v, want compaction followed by the newer fetch", got)
	}
}
