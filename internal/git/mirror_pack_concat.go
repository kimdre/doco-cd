package git

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/zlib"
	"crypto/sha1" // #nosec G505 -- the pack format mandates a SHA-1 trailer.
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// The consolidated pack is written by concatenating the packed entries of the
// existing packs instead of re-encoding the mirror's objects: re-encoding loads
// and delta-compresses every object again, which costs about ten times the
// packs' size in memory and minutes of CPU on larger mirrors. Copying keeps the
// compressed data, so the cost is bounded by disk throughput and memory only
// grows with the object count.
//
// An entry can only stay an OFS_DELTA when its base is copied from the same
// pack, since the new pack moves every entry; otherwise it becomes a
// REF_DELTA against its base's hash. Each object is copied from the first pack,
// oldest first, that holds it. A delta whose base comes from a later pack is
// written as a full object instead, so delta chains in the new pack only point
// to the same or older source packs and cannot form a cycle.

const (
	packSignature     = "PACK"
	packHeaderSize    = 12
	packChecksumSize  = sha1.Size
	packCopyBufSize   = 256 << 10
	maxEntryHeaderLen = 32

	// loosePackIndex marks objects that only exist as loose objects. They are
	// written as full objects after all packed entries.
	loosePackIndex = -1
)

// packSource is an existing pack whose entries are copied into the new pack.
type packSource struct {
	hash plumbing.Hash
	path string
	size int64
	// entries is sorted by offset.
	entries []idxfile.Entry
}

// keptObject records which copy of an object goes into the new pack.
type keptObject struct {
	// pack indexes the sources slice, or is loosePackIndex.
	pack int
	// offset is the object's offset in the new pack once written, or -1.
	offset int64
}

// packEntryHeader is a decoded pack entry header.
type packEntryHeader struct {
	// raw holds the header bytes as stored in the pack.
	raw  []byte
	size int64
	// baseOffset is the distance back to an OFS_DELTA's base.
	baseOffset int64
	// baseHash is a REF_DELTA's base.
	baseHash plumbing.Hash
	typ      plumbing.ObjectType
}

// packOutput writes the new pack while tracking its checksum, the current
// offset and the CRC-32 of the entry being written.
type packOutput struct {
	w      *bufio.Writer
	sum    hash.Hash
	crc    hash.Hash32
	offset int64
}

func (p *packOutput) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.sum.Write(b[:n])
	p.crc.Write(b[:n])
	p.offset += int64(n)

	return n, err
}

// startEntry resets the per-entry CRC and returns the entry's offset.
func (p *packOutput) startEntry() int64 {
	p.crc.Reset()
	return p.offset
}

// loadPackSources decodes the indexes of packs and returns them oldest first.
func loadPackSources(packDir string, packs []plumbing.Hash) ([]packSource, error) {
	type dated struct {
		packSource
		modTime int64
	}

	sources := make([]dated, 0, len(packs))

	for _, p := range packs {
		base := filepath.Join(packDir, "pack-"+p.String())

		info, err := os.Stat(base + ".pack")
		if err != nil {
			return nil, err
		}

		entries, err := readPackIndexEntries(base + ".idx")
		if err != nil {
			return nil, fmt.Errorf("read index of pack %s: %w", p, err)
		}

		if len(entries) > 0 && (entries[0].Offset != packHeaderSize || int64(entries[len(entries)-1].Offset) >= info.Size()-packChecksumSize) { // #nosec G115 -- pack offsets fit in int64.
			return nil, fmt.Errorf("pack %s: index offsets do not match the pack layout", p)
		}

		sources = append(sources, dated{
			packSource: packSource{hash: p, path: base + ".pack", size: info.Size(), entries: entries},
			modTime:    info.ModTime().UnixNano(),
		})
	}

	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i].modTime != sources[j].modTime {
			return sources[i].modTime < sources[j].modTime
		}

		return sources[i].hash.String() < sources[j].hash.String()
	})

	out := make([]packSource, len(sources))
	for i := range sources {
		out[i] = sources[i].packSource
	}

	return out, nil
}

// readPackIndexEntries returns the entries of a pack index sorted by offset.
func readPackIndexEntries(path string) ([]idxfile.Entry, error) {
	f, err := os.Open(path) // #nosec G304 -- path is built from the mirror directory and a pack checksum.
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	idx := idxfile.NewMemoryIndex()
	if err := idxfile.NewDecoder(bufio.NewReader(f)).Decode(idx); err != nil {
		return nil, err
	}

	iter, err := idx.Entries()
	if err != nil {
		return nil, err
	}

	defer func() { _ = iter.Close() }()

	var entries []idxfile.Entry

	for {
		e, err := iter.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, err
		}

		entries = append(entries, *e)
	}

	slices.SortFunc(entries, func(a, b idxfile.Entry) int { return cmp.Compare(a.Offset, b.Offset) })

	return entries, nil
}

// selectObjects picks the copy of every object that goes into the new pack:
// the one in the oldest pack holding it, or the loose object if no pack does.
// It returns the selection and the loose-only objects in a stable order.
func selectObjects(sources []packSource, loose []plumbing.Hash) (map[plumbing.Hash]keptObject, []plumbing.Hash) {
	kept := make(map[plumbing.Hash]keptObject)

	for i, src := range sources {
		for _, e := range src.entries {
			if _, ok := kept[e.Hash]; !ok {
				kept[e.Hash] = keptObject{pack: i, offset: -1}
			}
		}
	}

	var looseOnly []plumbing.Hash

	for _, h := range loose {
		if _, ok := kept[h]; !ok {
			kept[h] = keptObject{pack: loosePackIndex, offset: -1}
			looseOnly = append(looseOnly, h)
		}
	}

	slices.SortFunc(looseOnly, func(a, b plumbing.Hash) int { return bytes.Compare(a[:], b[:]) })

	return kept, looseOnly
}

// concatPacks writes every object of sources and loose into one new pack and
// its index in packDir and returns the new pack's checksum and object count.
func concatPacks(storage *filesystem.Storage, packDir string, sources []packSource, loose []plumbing.Hash) (plumbing.Hash, []plumbing.Hash, error) {
	kept, looseOnly := selectObjects(sources, loose)
	if len(kept) == 0 {
		return plumbing.ZeroHash, nil, nil
	}

	if uint64(len(kept)) > uint64(^uint32(0)) {
		return plumbing.ZeroHash, nil, fmt.Errorf("%d objects exceed the pack format limit", len(kept))
	}

	tmpPack, err := os.CreateTemp(packDir, tempPackPrefix)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("create temporary pack: %w", err)
	}

	tmpPackPath := tmpPack.Name()
	tmpIdxPath := ""

	defer func() {
		_ = tmpPack.Close()
		_ = os.Remove(tmpPackPath)

		if tmpIdxPath != "" {
			_ = os.Remove(tmpIdxPath)
		}
	}()

	out := &packOutput{
		w:   bufio.NewWriterSize(tmpPack, packCopyBufSize),
		sum: sha1.New(), // #nosec G401 -- the pack format mandates a SHA-1 trailer.
		crc: crc32.NewIEEE(),
	}

	var header [packHeaderSize]byte

	copy(header[:4], packSignature)
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(kept))) // #nosec G115 -- checked against the uint32 limit above.

	if _, err := out.Write(header[:]); err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("write pack header: %w", err)
	}

	idx := new(idxfile.Writer)
	if err := idx.OnHeader(uint32(len(kept))); err != nil { // #nosec G115 -- checked against the uint32 limit above.
		return plumbing.ZeroHash, nil, err
	}

	for i := range sources {
		if err := copyPackEntries(storage, out, idx, sources, i, kept); err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("copy pack %s: %w", sources[i].hash, err)
		}

		sources[i].entries = nil
	}

	for _, h := range looseOnly {
		if err := writeFullObject(storage, out, idx, kept, h); err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("write loose object %s: %w", h, err)
		}
	}

	var checksum plumbing.Hash

	copy(checksum[:], out.sum.Sum(nil))

	if _, err := out.w.Write(checksum[:]); err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("write pack checksum: %w", err)
	}

	if err := out.w.Flush(); err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("write pack: %w", err)
	}

	if err := tmpPack.Sync(); err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("sync pack: %w", err)
	}

	if err := tmpPack.Close(); err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("close pack: %w", err)
	}

	if err := idx.OnFooter(checksum); err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("build pack index: %w", err)
	}

	tmpIdxPath, err = writePackIndex(packDir, idx)
	if err != nil {
		return plumbing.ZeroHash, nil, err
	}

	base := filepath.Join(packDir, "pack-"+checksum.String())

	// go-git discovers packs by their .pack file, so the index must be in place
	// first: a pack without its index would break every read of the mirror.
	if err := os.Rename(tmpIdxPath, base+".idx"); err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("install pack index: %w", err)
	}

	tmpIdxPath = ""

	if err := os.Rename(tmpPackPath, base+".pack"); err != nil {
		_ = os.Remove(base + ".idx")
		return plumbing.ZeroHash, nil, fmt.Errorf("install pack: %w", err)
	}

	hashes := make([]plumbing.Hash, 0, len(kept))
	for h := range kept {
		hashes = append(hashes, h)
	}

	return checksum, hashes, nil
}

// writePackIndex encodes idx into a temporary file in packDir and returns its path.
func writePackIndex(packDir string, idx *idxfile.Writer) (string, error) {
	index, err := idx.Index()
	if err != nil {
		return "", fmt.Errorf("build pack index: %w", err)
	}

	f, err := os.CreateTemp(packDir, tempPackPrefix)
	if err != nil {
		return "", fmt.Errorf("create temporary pack index: %w", err)
	}

	w := bufio.NewWriter(f)

	_, err = idxfile.NewEncoder(w).Encode(index)
	if err == nil {
		err = w.Flush()
	}

	if err == nil {
		err = f.Sync()
	}

	if closeErr := f.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write pack index: %w", err)
	}

	return f.Name(), nil
}

// copyPackEntries copies the entries of sources[i] selected in kept to out.
// Every copied entry is checked against the CRC-32 in the source pack's index,
// so a corrupt source or a misread entry boundary aborts the compaction.
func copyPackEntries(storage *filesystem.Storage, out *packOutput, idx *idxfile.Writer, sources []packSource, i int, kept map[plumbing.Hash]keptObject) error {
	src := sources[i]

	f, err := os.Open(src.path)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	r := bufio.NewReaderSize(f, packCopyBufSize)

	var header [packHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return fmt.Errorf("read header: %w", err)
	}

	if string(header[:4]) != packSignature {
		return errors.New("not a packfile")
	}

	if v := binary.BigEndian.Uint32(header[4:8]); v != 2 && v != 3 {
		return fmt.Errorf("unsupported pack version %d", v)
	}

	// newOffsets maps this pack's entries to their offset in the new pack, for
	// OFS_DELTA entries whose base is copied along.
	newOffsets := make([]int64, len(src.entries))
	srcCRC := crc32.NewIEEE()
	pos := int64(packHeaderSize)
	headerBuf := make([]byte, 0, maxEntryHeaderLen)
	rewriteBuf := make([]byte, 0, maxEntryHeaderLen)
	copyBuf := make([]byte, packCopyBufSize)

	for j, e := range src.entries {
		offset := int64(e.Offset) // #nosec G115 -- pack offsets fit in int64.
		if offset != pos {
			return fmt.Errorf("entry %s at offset %d, want %d", e.Hash, offset, pos)
		}

		end := src.size - packChecksumSize
		if j+1 < len(src.entries) {
			end = int64(src.entries[j+1].Offset) // #nosec G115 -- pack offsets fit in int64.
		}

		length := end - offset
		pos = end
		newOffsets[j] = -1

		if kept[e.Hash].pack != i {
			if _, err := r.Discard(int(length)); err != nil {
				return fmt.Errorf("skip entry %s: %w", e.Hash, err)
			}

			continue
		}

		srcCRC.Reset()

		h, err := readPackEntryHeader(r, headerBuf[:0])
		if err != nil {
			return fmt.Errorf("read entry %s: %w", e.Hash, err)
		}

		srcCRC.Write(h.raw)

		dataLen := length - int64(len(h.raw))
		if dataLen <= 0 {
			return fmt.Errorf("entry %s: invalid length %d", e.Hash, length)
		}

		rewritten, full, err := rewriteEntryHeader(rewriteBuf[:0], h, offset, src, i, out.offset, newOffsets, kept)
		if err != nil {
			return fmt.Errorf("entry %s: %w", e.Hash, err)
		}

		if full {
			if _, err := io.CopyN(srcCRC, r, dataLen); err != nil {
				return fmt.Errorf("read entry %s: %w", e.Hash, err)
			}

			if srcCRC.Sum32() != e.CRC32 {
				return fmt.Errorf("entry %s: CRC-32 mismatch", e.Hash)
			}

			if err := writeFullObject(storage, out, idx, kept, e.Hash); err != nil {
				return fmt.Errorf("write entry %s as a full object: %w", e.Hash, err)
			}

			newOffsets[j] = kept[e.Hash].offset

			continue
		}

		newOffset := out.startEntry()

		if _, err := out.Write(rewritten); err != nil {
			return err
		}

		if err := copyEntryData(out, srcCRC, r, dataLen, copyBuf); err != nil {
			return fmt.Errorf("copy entry %s: %w", e.Hash, err)
		}

		if srcCRC.Sum32() != e.CRC32 {
			return fmt.Errorf("entry %s: CRC-32 mismatch", e.Hash)
		}

		idx.Add(e.Hash, uint64(newOffset), out.crc.Sum32()) // #nosec G115 -- offsets are never negative.
		kept[e.Hash] = keptObject{pack: i, offset: newOffset}
		newOffsets[j] = newOffset
	}

	return nil
}

// copyEntryData copies n bytes of entry data from r to out and srcCRC.
func copyEntryData(out *packOutput, srcCRC hash.Hash32, r io.Reader, n int64, buf []byte) error {
	for n > 0 {
		chunk := buf
		if int64(len(chunk)) > n {
			chunk = chunk[:n]
		}

		read, err := io.ReadFull(r, chunk)
		if err != nil {
			return err
		}

		srcCRC.Write(chunk[:read])

		if _, err := out.Write(chunk[:read]); err != nil {
			return err
		}

		n -= int64(read)
	}

	return nil
}

// rewriteEntryHeader appends to b the header to write for an entry of
// sources[i] at srcOffset that is about to be written at newOffset, or reports
// full when the entry must be written as a full object to keep delta chains
// acyclic.
func rewriteEntryHeader(b []byte, h packEntryHeader, srcOffset int64, src packSource, i int, newOffset int64, newOffsets []int64, kept map[plumbing.Hash]keptObject) ([]byte, bool, error) {
	switch h.typ {
	case plumbing.OFSDeltaObject:
		baseSrcOffset := srcOffset - h.baseOffset

		idx, found := slices.BinarySearchFunc(src.entries, baseSrcOffset, func(e idxfile.Entry, off int64) int {
			return cmp.Compare(int64(e.Offset), off) // #nosec G115 -- pack offsets fit in int64.
		})
		if !found {
			return nil, false, fmt.Errorf("no base object at offset %d", baseSrcOffset)
		}

		baseHash := src.entries[idx].Hash
		base := kept[baseHash]

		switch {
		case base.pack == i:
			if newOffsets[idx] < 0 {
				return nil, false, fmt.Errorf("base %s was not written before its delta", baseHash)
			}

			return appendOfsDeltaOffset(appendEntryHeader(b, plumbing.OFSDeltaObject, h.size), newOffset-newOffsets[idx]), false, nil
		case base.pack < i:
			return append(appendEntryHeader(b, plumbing.REFDeltaObject, h.size), baseHash[:]...), false, nil
		default:
			return nil, true, nil
		}
	case plumbing.REFDeltaObject:
		base, ok := kept[h.baseHash]
		if !ok {
			return nil, false, fmt.Errorf("base %s is not in the mirror", h.baseHash)
		}

		if base.pack > i {
			return nil, true, nil
		}

		return h.raw, false, nil
	case plumbing.CommitObject, plumbing.TreeObject, plumbing.BlobObject, plumbing.TagObject:
		return h.raw, false, nil
	default:
		return nil, false, fmt.Errorf("unsupported object type %d", h.typ)
	}
}

// writeFullObject writes the object h, resolved through storage, as a full
// (non-delta) entry.
func writeFullObject(storage *filesystem.Storage, out *packOutput, idx *idxfile.Writer, kept map[plumbing.Hash]keptObject, h plumbing.Hash) error {
	obj, err := storage.EncodedObject(plumbing.AnyObject, h)
	if err != nil {
		return err
	}

	r, err := obj.Reader()
	if err != nil {
		return err
	}

	defer func() { _ = r.Close() }()

	newOffset := out.startEntry()

	if _, err := out.Write(appendEntryHeader(nil, obj.Type(), obj.Size())); err != nil {
		return err
	}

	zw := zlib.NewWriter(out)

	if _, err := io.Copy(zw, r); err != nil {
		return err
	}

	if err := zw.Close(); err != nil {
		return err
	}

	idx.Add(h, uint64(newOffset), out.crc.Sum32()) // #nosec G115 -- offsets are never negative.

	k := kept[h]
	k.offset = newOffset
	kept[h] = k

	return nil
}

// readPackEntryHeader reads the type, size and delta base of a pack entry. The
// returned raw header is appended to buf.
func readPackEntryHeader(r *bufio.Reader, buf []byte) (packEntryHeader, error) {
	h := packEntryHeader{raw: buf}

	c, err := r.ReadByte()
	if err != nil {
		return h, err
	}

	h.raw = append(h.raw, c)
	h.typ = plumbing.ObjectType((c >> 4) & 0x07)
	h.size = int64(c & 0x0f)

	for shift := 4; c&0x80 != 0; shift += 7 {
		if shift > 63 {
			return h, errors.New("object size overflows")
		}

		if c, err = r.ReadByte(); err != nil {
			return h, err
		}

		h.raw = append(h.raw, c)
		h.size |= int64(c&0x7f) << shift
	}

	switch h.typ {
	case plumbing.OFSDeltaObject:
		if c, err = r.ReadByte(); err != nil {
			return h, err
		}

		h.raw = append(h.raw, c)
		h.baseOffset = int64(c & 0x7f)

		for c&0x80 != 0 {
			if h.baseOffset >= 1<<56 {
				return h, errors.New("delta base offset overflows")
			}

			if c, err = r.ReadByte(); err != nil {
				return h, err
			}

			h.raw = append(h.raw, c)
			h.baseOffset = ((h.baseOffset + 1) << 7) | int64(c&0x7f)
		}

		if h.baseOffset <= 0 {
			return h, errors.New("invalid delta base offset")
		}
	case plumbing.REFDeltaObject:
		if _, err := io.ReadFull(r, h.baseHash[:]); err != nil {
			return h, err
		}

		h.raw = append(h.raw, h.baseHash[:]...)
	}

	return h, nil
}

// appendEntryHeader appends a pack entry's type and size header to b.
func appendEntryHeader(b []byte, typ plumbing.ObjectType, size int64) []byte {
	c := byte(typ)<<4 | byte(size&0x0f) // #nosec G115 -- pack object types are 3-bit values.

	for size >>= 4; size != 0; size >>= 7 {
		b = append(b, c|0x80)
		c = byte(size & 0x7f)
	}

	return append(b, c)
}

// appendOfsDeltaOffset appends an OFS_DELTA base offset to b.
func appendOfsDeltaOffset(b []byte, offset int64) []byte {
	var buf [10]byte

	n := len(buf) - 1
	buf[n] = byte(offset & 0x7f)

	for offset >>= 7; offset != 0; offset >>= 7 {
		offset--
		n--
		buf[n] = 0x80 | byte(offset&0x7f)
	}

	return append(b, buf[n:]...)
}
