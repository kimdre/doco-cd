package filesystem

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// contentCompareBufferSize is the chunk size used to compare regular files.
const contentCompareBufferSize = 64 * 1024

// ContentEqual reports whether the file system entries at a and b have identical content.
//
// Regular files are equal when they have the same permission bits and bytes, symlinks when they
// point to the same target (targets are never followed) and directories when they have the same
// permission bits and contain equal entries under the same names. Other file types (sockets,
// devices, named pipes) are never equal.
//
// A path that is missing on one side is equal to a missing path or to a directory tree that
// only consists of directories on the other side. Docker creates missing bind-mount sources and
// mount points of nested mounts as empty directories on the host, so such a tree carries no
// content of its own.
func ContentEqual(a, b string) (bool, error) {
	aInfo, aMissing, err := lstatIfExists(a)
	if err != nil {
		return false, err
	}

	bInfo, bMissing, err := lstatIfExists(b)
	if err != nil {
		return false, err
	}

	switch {
	case aMissing && bMissing:
		return true, nil
	case aMissing:
		return isDirOnlyTree(b, bInfo)
	case bMissing:
		return isDirOnlyTree(a, aInfo)
	}

	if aInfo.Mode().Type() != bInfo.Mode().Type() || comparableMode(aInfo) != comparableMode(bInfo) {
		return false, nil
	}

	switch aInfo.Mode().Type() {
	case 0:
		if os.SameFile(aInfo, bInfo) {
			return true, nil
		}

		if aInfo.Size() != bInfo.Size() {
			return false, nil
		}

		return regularFilesEqual(a, b)
	case fs.ModeSymlink:
		aTarget, err := os.Readlink(a)
		if err != nil {
			return false, fmt.Errorf("read symlink %s: %w", a, err)
		}

		bTarget, err := os.Readlink(b)
		if err != nil {
			return false, fmt.Errorf("read symlink %s: %w", b, err)
		}

		return aTarget == bTarget, nil
	case fs.ModeDir:
		return dirsEqual(a, b)
	default:
		return false, nil
	}
}

func lstatIfExists(path string) (fs.FileInfo, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, true, nil
		}

		return nil, false, fmt.Errorf("stat %s: %w", path, err)
	}

	return info, false, nil
}

func comparableMode(info fs.FileInfo) fs.FileMode {
	return info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

func dirsEqual(a, b string) (bool, error) {
	aEntries, err := os.ReadDir(a)
	if err != nil {
		return false, fmt.Errorf("read directory %s: %w", a, err)
	}

	bEntries, err := os.ReadDir(b)
	if err != nil {
		return false, fmt.Errorf("read directory %s: %w", b, err)
	}

	names := make([]string, 0, len(aEntries)+len(bEntries))
	for _, entry := range aEntries {
		names = append(names, entry.Name())
	}

	for _, entry := range bEntries {
		names = append(names, entry.Name())
	}

	slices.Sort(names)

	for _, name := range slices.Compact(names) {
		equal, err := ContentEqual(filepath.Join(a, name), filepath.Join(b, name))
		if err != nil || !equal {
			return false, err
		}
	}

	return true, nil
}

// isDirOnlyTree reports whether path is a directory that, recursively, only contains directories.
func isDirOnlyTree(path string, info fs.FileInfo) (bool, error) {
	if !info.IsDir() {
		return false, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return false, fmt.Errorf("read directory %s: %w", path, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			return false, nil
		}

		entryInfo, err := entry.Info()
		if err != nil {
			return false, fmt.Errorf("stat %s: %w", filepath.Join(path, entry.Name()), err)
		}

		empty, err := isDirOnlyTree(filepath.Join(path, entry.Name()), entryInfo)
		if err != nil || !empty {
			return false, err
		}
	}

	return true, nil
}

func regularFilesEqual(a, b string) (bool, error) {
	fileA, err := os.Open(a)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", a, err)
	}

	defer func() { _ = fileA.Close() }()

	fileB, err := os.Open(b)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", b, err)
	}

	defer func() { _ = fileB.Close() }()

	aBuf := make([]byte, contentCompareBufferSize)
	bBuf := make([]byte, contentCompareBufferSize)

	for {
		aRead, aErr := io.ReadFull(fileA, aBuf)
		bRead, bErr := io.ReadFull(fileB, bBuf)

		if !bytes.Equal(aBuf[:aRead], bBuf[:bRead]) {
			return false, nil
		}

		aDone := errors.Is(aErr, io.EOF) || errors.Is(aErr, io.ErrUnexpectedEOF)
		bDone := errors.Is(bErr, io.EOF) || errors.Is(bErr, io.ErrUnexpectedEOF)

		if aErr != nil && !aDone {
			return false, fmt.Errorf("read %s: %w", a, aErr)
		}

		if bErr != nil && !bDone {
			return false, fmt.Errorf("read %s: %w", b, bErr)
		}

		if aDone || bDone {
			return aDone == bDone, nil
		}
	}
}
