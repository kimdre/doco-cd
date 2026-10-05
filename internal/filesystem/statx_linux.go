//go:build linux

package filesystem

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// BirthTime returns the time path was created. It reports false if the file system does not record it.
func BirthTime(path string) (time.Time, bool, error) {
	stx, err := statx(path, unix.STATX_BTIME)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) {
			return time.Time{}, false, nil
		}

		return time.Time{}, false, err
	}

	if stx.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}, false, nil
	}

	return time.Unix(stx.Btime.Sec, int64(stx.Btime.Nsec)), true, nil
}

// Identity returns a string that identifies the file at path: its inode number and, if the file system records it,
// its creation time. Both survive renaming the file, while a file re-created at the same path gets a different identity.
func Identity(path string) (string, error) {
	stx, err := statx(path, unix.STATX_INO|unix.STATX_BTIME)
	if err != nil {
		if !errors.Is(err, unix.ENOSYS) {
			return "", err
		}

		info, statErr := os.Stat(path)
		if statErr != nil {
			return "", statErr
		}

		sys, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return "", nil
		}

		return strconv.FormatUint(sys.Ino, 10), nil
	}

	identity := strconv.FormatUint(stx.Ino, 10)
	if stx.Mask&unix.STATX_BTIME != 0 {
		identity += ":" + strconv.FormatInt(time.Unix(stx.Btime.Sec, int64(stx.Btime.Nsec)).UnixNano(), 10)
	}

	return identity, nil
}

func statx(path string, mask int) (unix.Statx_t, error) {
	var stx unix.Statx_t

	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_STATX_SYNC_AS_STAT, mask, &stx); err != nil {
		return stx, &fs.PathError{Op: "statx", Path: path, Err: err}
	}

	return stx, nil
}
