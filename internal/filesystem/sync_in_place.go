package filesystem

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// SyncInPlace updates the file system tree at dst to match the tree at src, without removing
// entries of dst that do not exist in src.
//
// Existing regular files are overwritten in place instead of being replaced, so their inodes are
// kept: a file bind-mounted into a container references the inode of its source, and replacing the
// file (e.g. by renaming a new file over it) would hide the new content from the container.
// Entries whose type differs between src and dst are replaced. Symlinks are copied, never followed.
// Other file types (sockets, devices, named pipes) are skipped. A missing src leaves dst untouched.
//
// It returns the slash-separated paths relative to src ("." for src itself) of all entries copied
// from src, parents before their children, and of the entries it created or modified.
func SyncInPlace(src, dst string) (entries, changed []string, err error) {
	err = syncInPlace(src, dst, ".", &entries, &changed)

	return entries, changed, err
}

func syncInPlace(src, dst, rel string, entries, changed *[]string) error {
	srcInfo, srcMissing, err := lstatIfExists(src)
	if err != nil || srcMissing {
		return err
	}

	dstInfo, dstMissing, err := lstatIfExists(dst)
	if err != nil {
		return err
	}

	srcType := srcInfo.Mode().Type()
	if srcType != 0 && srcType != fs.ModeSymlink && srcType != fs.ModeDir {
		return nil
	}

	if !dstMissing && dstInfo.Mode().Type() != srcType {
		if err = os.RemoveAll(dst); err != nil {
			return fmt.Errorf("remove %s: %w", dst, err)
		}

		dstMissing = true
	}

	*entries = append(*entries, rel)

	var modified bool

	switch srcType {
	case 0:
		modified, err = syncRegularFile(src, dst, srcInfo, dstInfo, dstMissing)
	case fs.ModeSymlink:
		modified, err = syncSymlink(src, dst, dstMissing)
	case fs.ModeDir:
		modified, err = syncDir(src, dst, rel, srcInfo, dstInfo, dstMissing, entries, changed)
	}

	if err != nil {
		return err
	}

	if modified {
		*changed = append(*changed, rel)
	}

	return nil
}

func syncRegularFile(src, dst string, srcInfo, dstInfo fs.FileInfo, dstMissing bool) (bool, error) {
	mode := comparableMode(srcInfo)

	if !dstMissing && dstInfo.Size() == srcInfo.Size() {
		equal, err := regularFilesEqual(src, dst)
		if err != nil {
			return false, err
		}

		if equal {
			if comparableMode(dstInfo) == mode {
				return false, nil
			}

			if err = os.Chmod(dst, mode); err != nil {
				return false, fmt.Errorf("chmod %s: %w", dst, err)
			}

			return true, nil
		}
	}

	// The owner may always change the mode of its files, so make a read-only file writable first.
	if !dstMissing && dstInfo.Mode().Perm()&0o200 == 0 {
		if err := os.Chmod(dst, dstInfo.Mode().Perm()|0o200); err != nil {
			return false, fmt.Errorf("chmod %s: %w", dst, err)
		}
	}

	if err := copyFileContent(src, dst, mode.Perm()); err != nil {
		return false, err
	}

	if err := os.Chmod(dst, mode); err != nil {
		return false, fmt.Errorf("chmod %s: %w", dst, err)
	}

	return true, nil
}

func copyFileContent(src, dst string, perm fs.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}

	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("open %s: %w", dst, err)
	}

	defer func() {
		if closeErr := out.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", dst, closeErr)
		}
	}()

	if _, err = io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}

	return nil
}

func syncSymlink(src, dst string, dstMissing bool) (bool, error) {
	target, err := os.Readlink(src)
	if err != nil {
		return false, fmt.Errorf("read symlink %s: %w", src, err)
	}

	if !dstMissing {
		current, err := os.Readlink(dst)
		if err != nil {
			return false, fmt.Errorf("read symlink %s: %w", dst, err)
		}

		if current == target {
			return false, nil
		}

		if err = os.Remove(dst); err != nil {
			return false, fmt.Errorf("remove %s: %w", dst, err)
		}
	}

	if err = os.Symlink(target, dst); err != nil {
		return false, fmt.Errorf("create symlink %s: %w", dst, err)
	}

	return true, nil
}

func syncDir(src, dst, rel string, srcInfo, dstInfo fs.FileInfo, dstMissing bool, entries, changed *[]string) (bool, error) {
	mode := comparableMode(srcInfo)
	modified := dstMissing || comparableMode(dstInfo) != mode

	// The owner needs full access to populate the directory; the final mode is applied afterwards.
	restore := false

	if dstMissing {
		if err := os.Mkdir(dst, PermDir); err != nil && !errors.Is(err, fs.ErrExist) {
			return false, fmt.Errorf("create directory %s: %w", dst, err)
		}
	} else if perm := dstInfo.Mode().Perm(); perm&0o700 != 0o700 {
		if err := os.Chmod(dst, perm|0o700); err != nil {
			return false, fmt.Errorf("chmod %s: %w", dst, err)
		}

		restore = true
	}

	children, err := os.ReadDir(src)
	if err != nil {
		return false, fmt.Errorf("read directory %s: %w", src, err)
	}

	for _, child := range children {
		name := child.Name()
		if err = syncInPlace(filepath.Join(src, name), filepath.Join(dst, name), pathJoinRel(rel, name), entries, changed); err != nil {
			return false, err
		}
	}

	if modified || restore {
		if err = os.Chmod(dst, mode); err != nil {
			return false, fmt.Errorf("chmod %s: %w", dst, err)
		}
	}

	return modified, nil
}

func pathJoinRel(rel, name string) string {
	if rel == "." {
		return name
	}

	return rel + "/" + name
}
