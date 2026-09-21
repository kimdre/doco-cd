package git

import (
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5"

	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

// ErrMissingMirrorDir is returned when a mirror read is attempted without knowing
// which bare mirror to read from. Falling back to an unlocked, unscoped read would
// silently drop both the cross-process exclusion and the freshness guarantee that
// WithMirrorRead exists to provide, so it is reported instead.
var ErrMissingMirrorDir = errors.New("mirror directory is empty")

// WithMirrorRead runs fn against a freshly opened handle on the bare mirror at
// mirrorDir while holding that mirror's shared path lock, then releases both.
//
// Both halves matter, and neither is sufficient alone:
//
//   - The shared lock excludes a concurrent stack's fetch, which takes the matching
//     exclusive lock in CloneOrUpdateBareMirror/GitStore.Publish. It keeps the pack
//     directory stable for the duration of the read.
//   - The *fresh* handle is what makes the lock effective. go-git's
//     filesystem.ObjectStorage caches its packfile index map on first use and never
//     refreshes it (requireIndex returns early once s.index != nil), while
//     dotgit.ObjectPacks re-reads the pack directory on every call. A handle that
//     was opened before some other job fetched therefore enumerates a packfile it
//     has no index for, and go-git dereferences that nil index as a nil
//     idxfile.Index interface inside packfile.GetByType - a SIGSEGV, not an error.
//
// The handle is scoped to fn precisely so it cannot outlive the lock and become
// stale again: every read must re-enter through this helper rather than retain a
// long-lived *git.Repository across lock regions.
//
// fn may perform any number of reads; batching them into a single call keeps the
// handle and the lock to one acquisition per logical read region.
func WithMirrorRead(mirrorDir string, fn func(repo *git.Repository) error) error {
	if mirrorDir == "" {
		return ErrMissingMirrorDir
	}

	if fn == nil {
		return errors.New("mirror read function is nil")
	}

	unlock := sourcecache.AcquireSharedPathLock(mirrorDir)
	defer unlock()

	repo, err := git.PlainOpen(mirrorDir)
	if err != nil {
		return fmt.Errorf("failed to open git mirror at %s: %w", mirrorDir, err)
	}

	return fn(repo)
}

// MirrorRead is the value-returning form of WithMirrorRead, for the common case of
// a read region that produces a single result. On error it returns the zero value
// of T alongside the error.
func MirrorRead[T any](mirrorDir string, fn func(repo *git.Repository) (T, error)) (T, error) {
	if fn == nil {
		var zero T

		return zero, errors.New("mirror read function is nil")
	}

	var result T

	err := WithMirrorRead(mirrorDir, func(repo *git.Repository) error {
		var readErr error

		result, readErr = fn(repo)

		return readErr
	})
	if err != nil {
		var zero T

		return zero, err
	}

	return result, nil
}
