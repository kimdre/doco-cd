package git

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1" // #nosec G505 -- the pack format mandates a SHA-1 trailer.
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// testPackEntry describes an entry of a pack built by writeTestPack.
type testPackEntry struct {
	hash plumbing.Hash
	typ  plumbing.ObjectType
	// data is the object content, or the delta for delta entries.
	data []byte
	// refBase is a REF_DELTA's base.
	refBase plumbing.Hash
	// ofsBase indexes an OFS_DELTA's base among the preceding entries.
	ofsBase int
}

type testBlob struct {
	hash    plumbing.Hash
	content []byte
}

func newTestBlob(content string) testBlob {
	return testBlob{hash: plumbing.ComputeHash(plumbing.BlobObject, []byte(content)), content: []byte(content)}
}

func fullEntry(b testBlob) testPackEntry {
	return testPackEntry{hash: b.hash, typ: plumbing.BlobObject, data: b.content}
}

func refDeltaEntry(b, base testBlob) testPackEntry {
	return testPackEntry{hash: b.hash, typ: plumbing.REFDeltaObject, data: packfile.DiffDelta(base.content, b.content), refBase: base.hash}
}

func ofsDeltaEntry(b, base testBlob, baseIndex int) testPackEntry {
	return testPackEntry{hash: b.hash, typ: plumbing.OFSDeltaObject, data: packfile.DiffDelta(base.content, b.content), ofsBase: baseIndex}
}

// writeTestPack writes a pack holding entries, and its index, into packDir and
// sets the pack's modification time to modTime.
func writeTestPack(t *testing.T, packDir string, modTime time.Time, entries ...testPackEntry) plumbing.Hash {
	t.Helper()

	var pack bytes.Buffer

	header := make([]byte, packHeaderSize)
	copy(header, packSignature)
	binary.BigEndian.PutUint32(header[4:], 2)
	binary.BigEndian.PutUint32(header[8:], uint32(len(entries))) // #nosec G115 -- small test packs.
	_, _ = pack.Write(header)

	idx := new(idxfile.Writer)
	_ = idx.OnHeader(uint32(len(entries))) // #nosec G115 -- small test packs.

	offsets := make([]int64, len(entries))

	for i, e := range entries {
		offsets[i] = int64(pack.Len())

		raw := appendEntryHeader(nil, e.typ, int64(len(e.data)))

		switch e.typ {
		case plumbing.OFSDeltaObject:
			raw = appendOfsDeltaOffset(raw, offsets[i]-offsets[e.ofsBase])
		case plumbing.REFDeltaObject:
			raw = append(raw, e.refBase[:]...)
		}

		var compressed bytes.Buffer

		zw := zlib.NewWriter(&compressed)
		_, _ = zw.Write(e.data)
		_ = zw.Close()

		raw = append(raw, compressed.Bytes()...)
		_, _ = pack.Write(raw)
		idx.Add(e.hash, uint64(offsets[i]), crc32.ChecksumIEEE(raw)) // #nosec G115 -- offsets are never negative.
	}

	sum := sha1.Sum(pack.Bytes()) // #nosec G401 -- the pack format mandates a SHA-1 trailer.
	_, _ = pack.Write(sum[:])

	checksum := plumbing.Hash(sum)
	if err := idx.OnFooter(checksum); err != nil {
		t.Fatalf("build index: %v", err)
	}

	index, err := idx.Index()
	if err != nil {
		t.Fatalf("build index: %v", err)
	}

	var idxBuf bytes.Buffer
	if _, err := idxfile.NewEncoder(&idxBuf).Encode(index); err != nil {
		t.Fatalf("encode index: %v", err)
	}

	base := filepath.Join(packDir, "pack-"+checksum.String())

	if err := os.WriteFile(base+".idx", idxBuf.Bytes(), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}

	if err := os.WriteFile(base+".pack", pack.Bytes(), 0o600); err != nil {
		t.Fatalf("write pack: %v", err)
	}

	if err := os.Chtimes(base+".pack", modTime, modTime); err != nil {
		t.Fatalf("set pack time: %v", err)
	}

	return checksum
}

// newBareTestRepo returns an empty bare repository and its pack directory.
func newBareTestRepo(t *testing.T) (string, *filesystem.Storage, string) {
	t.Helper()

	path := t.TempDir()

	repo, err := gogit.PlainInit(path, true)
	if err != nil {
		t.Fatalf("init bare repo: %v", err)
	}

	packDir := filepath.Join(path, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o750); err != nil {
		t.Fatalf("create pack dir: %v", err)
	}

	return path, repo.Storer.(*filesystem.Storage), packDir
}

// packEntryTypes returns the stored entry type of every object in the single
// pack in packDir.
func packEntryTypes(t *testing.T, packDir string) map[plumbing.Hash]plumbing.ObjectType {
	t.Helper()

	idxFiles, err := filepath.Glob(filepath.Join(packDir, "pack-*.idx"))
	if err != nil || len(idxFiles) != 1 {
		t.Fatalf("pack indexes = %v (%v), want exactly one", idxFiles, err)
	}

	entries, err := readPackIndexEntries(idxFiles[0])
	if err != nil {
		t.Fatalf("read index: %v", err)
	}

	f, err := os.Open(strings.TrimSuffix(idxFiles[0], ".idx") + ".pack")
	if err != nil {
		t.Fatalf("open pack: %v", err)
	}

	defer func() { _ = f.Close() }()

	types := make(map[plumbing.Hash]plumbing.ObjectType, len(entries))

	for _, e := range entries {
		h, err := readPackEntryHeader(bufio.NewReader(io.NewSectionReader(f, int64(e.Offset), maxEntryHeaderLen)), nil) // #nosec G115 -- small test packs.
		if err != nil {
			t.Fatalf("read entry %s: %v", e.Hash, err)
		}

		types[e.Hash] = h.typ
	}

	return types
}

func readBlob(t *testing.T, path string, h plumbing.Hash) []byte {
	t.Helper()

	repo, err := gogit.PlainOpen(path)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}

	blob, err := repo.BlobObject(h)
	if err != nil {
		t.Fatalf("read blob %s: %v", h, err)
	}

	r, err := blob.Reader()
	if err != nil {
		t.Fatalf("read blob %s: %v", h, err)
	}

	defer func() { _ = r.Close() }()

	content, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read blob %s: %v", h, err)
	}

	return content
}

// gitFsck checks the repository with the git CLI when it is installed.
func gitFsck(t *testing.T, path string) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		return
	}

	out, err := exec.Command("git", "--git-dir="+path, "fsck", "--full", "--no-dangling").CombinedOutput() // #nosec G204 -- test-controlled path.
	if err != nil {
		t.Fatalf("git fsck: %v\n%s", err, out)
	}
}

func TestConsolidatePacks_RewritesDeltasAcrossPacks(t *testing.T) {
	t.Parallel()

	path, storage, packDir := newBareTestRepo(t)

	text := strings.Repeat("the quick brown fox jumps over the lazy dog\n", 40)
	a := newTestBlob(text + "version a\n")
	b := newTestBlob(text + "version b\n")
	c := newTestBlob(text + "version c\n")
	x := newTestBlob(text + "version x\n")
	y := newTestBlob(text + "version y\n")

	now := time.Now()

	// The oldest pack holds y as a delta against x, which only a newer pack
	// holds, where x is in turn a delta against y. Copying both entries as-is
	// would create a delta cycle.
	oldest := writeTestPack(t, packDir, now.Add(-3*time.Hour), fullEntry(a), refDeltaEntry(y, x))
	// a is copied from the oldest pack, so b's OFS_DELTA has to become a
	// REF_DELTA, while c's base b is copied along with it.
	middle := writeTestPack(t, packDir, now.Add(-2*time.Hour), fullEntry(a), ofsDeltaEntry(b, a, 0), ofsDeltaEntry(c, b, 1))
	newest := writeTestPack(t, packDir, now.Add(-time.Hour), fullEntry(y), refDeltaEntry(x, y))

	loose := storage.NewEncodedObject()
	loose.SetType(plumbing.BlobObject)

	w, _ := loose.Writer()
	_, _ = w.Write([]byte("loose\n"))
	_ = w.Close()

	looseHash, err := storage.SetEncodedObject(loose)
	if err != nil {
		t.Fatalf("store loose object: %v", err)
	}

	result, err := consolidatePacks(storage, packDir, []plumbing.Hash{newest, oldest, middle})
	if err != nil || !result.written {
		t.Fatalf("consolidatePacks() = %+v, %v, want written, nil", result, err)
	}

	want := map[plumbing.Hash]plumbing.ObjectType{
		a.hash:    plumbing.BlobObject,
		b.hash:    plumbing.REFDeltaObject,
		c.hash:    plumbing.OFSDeltaObject,
		y.hash:    plumbing.BlobObject,
		x.hash:    plumbing.REFDeltaObject,
		looseHash: plumbing.BlobObject,
	}

	if result.objects != len(want) || result.looseObjects != 1 {
		t.Fatalf("consolidatePacks() counted %d objects and %d loose objects, want %d and 1",
			result.objects, result.looseObjects, len(want))
	}

	got := packEntryTypes(t, packDir)
	if len(got) != len(want) {
		t.Fatalf("new pack holds %d objects, want %d", len(got), len(want))
	}

	for h, typ := range want {
		if got[h] != typ {
			t.Errorf("object %s stored as %s, want %s", h, got[h], typ)
		}
	}

	for _, blob := range []testBlob{a, b, c, x, y} {
		if content := readBlob(t, path, blob.hash); !bytes.Equal(content, blob.content) {
			t.Errorf("blob %s content = %q, want %q", blob.hash, content, blob.content)
		}
	}

	if content := readBlob(t, path, looseHash); string(content) != "loose\n" {
		t.Errorf("formerly loose blob content = %q", content)
	}

	gitFsck(t, path)
}

func TestConsolidatePacks_RebuildsDeltaAgainstNewerPack(t *testing.T) {
	t.Parallel()

	path, storage, packDir := newBareTestRepo(t)

	text := strings.Repeat("pack my box with five dozen liquor jugs\n", 40)
	x := newTestBlob(text + "version x\n")
	y := newTestBlob(text + "version y\n")

	now := time.Now()

	// y only exists as a delta against x, which only a newer pack holds. go-git
	// cannot read y at all, since it never resolves a delta base from another
	// pack, so y has to be rebuilt from its delta to be written as a full object.
	older := writeTestPack(t, packDir, now.Add(-2*time.Hour), refDeltaEntry(y, x))
	newer := writeTestPack(t, packDir, now.Add(-time.Hour), fullEntry(x))

	result, err := consolidatePacks(storage, packDir, []plumbing.Hash{newer, older})
	if err != nil || !result.written {
		t.Fatalf("consolidatePacks() = %+v, %v, want written, nil", result, err)
	}

	want := map[plumbing.Hash]plumbing.ObjectType{
		x.hash: plumbing.BlobObject,
		y.hash: plumbing.BlobObject,
	}

	got := packEntryTypes(t, packDir)
	if len(got) != len(want) {
		t.Fatalf("new pack holds %d objects, want %d", len(got), len(want))
	}

	for h, typ := range want {
		if got[h] != typ {
			t.Errorf("object %s stored as %s, want %s", h, got[h], typ)
		}
	}

	for _, blob := range []testBlob{x, y} {
		if content := readBlob(t, path, blob.hash); !bytes.Equal(content, blob.content) {
			t.Errorf("blob %s content = %q, want %q", blob.hash, content, blob.content)
		}
	}

	gitFsck(t, path)
}

func TestConsolidatePacks_KeepsPacksWhenSourceIsCorrupt(t *testing.T) {
	t.Parallel()

	path, storage, packDir := newBareTestRepo(t)

	text := strings.Repeat("lorem ipsum dolor sit amet\n", 40)
	a := newTestBlob(text + "a\n")
	b := newTestBlob(text + "b\n")

	now := time.Now()
	first := writeTestPack(t, packDir, now.Add(-2*time.Hour), fullEntry(a))
	second := writeTestPack(t, packDir, now.Add(-time.Hour), fullEntry(b))

	// Flip a byte inside b's compressed data, which only the index's CRC-32
	// can detect without inflating the entry.
	secondPack := filepath.Join(packDir, "pack-"+second.String()+".pack")

	data, err := os.ReadFile(secondPack)
	if err != nil {
		t.Fatalf("read pack: %v", err)
	}

	data[len(data)-packChecksumSize-2] ^= 0xff

	if err := os.WriteFile(secondPack, data, 0o600); err != nil { //nolint:gosec // secondPack is inside the test's temp dir.
		t.Fatalf("write pack: %v", err)
	}

	result, err := consolidatePacks(storage, packDir, []plumbing.Hash{first, second})
	if err == nil || !strings.Contains(err.Error(), "CRC-32 mismatch") {
		t.Fatalf("consolidatePacks() error = %v, want a CRC-32 mismatch", err)
	}

	if result.written {
		t.Fatal("consolidatePacks() reported a changed pack directory after failing")
	}

	if got := countPacks(t, path); got != 2 {
		t.Fatalf("mirror holds %d packs, want the original 2", got)
	}

	leftovers, _ := filepath.Glob(filepath.Join(packDir, tempPackPrefix+"*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestPackEntryHeader_RoundTrip(t *testing.T) {
	t.Parallel()

	for _, size := range []int64{0, 15, 16, 127, 128, 1 << 20, 1<<35 + 7} {
		for _, ofs := range []int64{1, 127, 128, 16511, 16512, 1 << 30} {
			raw := appendOfsDeltaOffset(appendEntryHeader(nil, plumbing.OFSDeltaObject, size), ofs)

			h, err := readPackEntryHeader(bufio.NewReader(bytes.NewReader(raw)), nil)
			if err != nil {
				t.Fatalf("size %d offset %d: %v", size, ofs, err)
			}

			if h.typ != plumbing.OFSDeltaObject || h.size != size || h.baseOffset != ofs || !bytes.Equal(h.raw, raw) {
				t.Fatalf("size %d offset %d: decoded %+v", size, ofs, h)
			}
		}
	}
}
