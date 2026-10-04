package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
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
