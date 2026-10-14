package cache

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestContextSourceLocksCancelWhileContended(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		hold    func(string) (func(), error)
		acquire func(context.Context, string) (func(), error)
	}{
		{
			name: "path in-process",
			hold: func(path string) (func(), error) {
				return AcquirePathLock(path), nil
			},
			acquire: AcquirePathLockContext,
		},
		{
			name: "path cross-process",
			hold: func(path string) (func(), error) {
				return acquireRequiredCrossProcessLock(path, unix.LOCK_EX)
			},
			acquire: AcquirePathLockContext,
		},
		{
			name:    "GC in-process",
			hold:    AcquireExclusiveGCPathLock,
			acquire: AcquireSharedGCPathLockContext,
		},
		{
			name: "GC cross-process",
			hold: func(path string) (func(), error) {
				return acquireRequiredCrossProcessLock(path+gcLockSuffix, unix.LOCK_EX)
			},
			acquire: AcquireSharedGCPathLockContext,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "source")

			unlock, err := tc.hold(path)
			if err != nil {
				t.Fatal(err)
			}

			unlock = sync.OnceFunc(unlock)
			defer unlock()

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()

			release, err := tc.acquire(ctx, path)
			if release != nil {
				release()
				t.Fatal("acquired a contended source lock")
			}

			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lock error = %v, want context.DeadlineExceeded", err)
			}

			unlock()

			retryCtx, retryCancel := context.WithTimeout(t.Context(), time.Second)
			defer retryCancel()

			release, err = tc.acquire(retryCtx, path)
			if err != nil {
				t.Fatalf("acquire after the holder released: %v", err)
			}

			release()
			release()
		})
	}
}

func TestContextSourceLocksRejectCanceledContext(t *testing.T) {
	t.Parallel()

	for _, acquire := range []func(context.Context, string) (func(), error){
		AcquirePathLockContext,
		AcquireSharedGCPathLockContext,
	} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		release, err := acquire(ctx, filepath.Join(t.TempDir(), "source"))
		if release != nil {
			release()
			t.Fatal("acquired a source lock with a canceled context")
		}

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lock error = %v, want context.Canceled", err)
		}
	}
}

func TestSharedGCPathLockContextReleasesAncestorsOnCancellation(t *testing.T) {
	root := t.TempDir()

	restore := SetSourceRoot(root)
	defer restore()

	parent := filepath.Join(root, "parent")
	path := filepath.Join(parent, "child")

	unlock, err := AcquireExclusiveGCPathLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	release, err := AcquireSharedGCPathLockContext(ctx, path)
	if release != nil {
		release()
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock error = %v, want context.DeadlineExceeded", err)
	}

	unlockParent, acquired, err := tryAcquireTreeLock(parent, true)
	if err != nil || !acquired {
		t.Fatalf("ancestor remained locked after cancellation: acquired=%v, err=%v", acquired, err)
	}

	unlockParent()
}

func TestSharedGCPathLockContextCancelsAtAncestor(t *testing.T) {
	root := t.TempDir()

	restore := SetSourceRoot(root)
	defer restore()

	parent := filepath.Join(root, "parent")

	unlock, acquired, err := tryAcquireTreeLock(parent, true)
	if err != nil || !acquired {
		t.Fatalf("hold ancestor: acquired=%v, err=%v", acquired, err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	release, err := AcquireSharedGCPathLockContext(ctx, filepath.Join(parent, "child"))
	if release != nil {
		release()
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock error = %v, want context.DeadlineExceeded", err)
	}
}
