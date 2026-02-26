package git

import (
	"testing"
	"time"
)

func TestLockForPath(t *testing.T) {
	path := "test/repo/path"

	lock1 := lockForKey(path)
	lock2 := lockForKey(path)

	if lock1 != lock2 {
		t.Fatalf("expected same mutex for the same path, got different instances")
	}
}

func TestAcquirePathLock(t *testing.T) {
	path := "test/repo/path"

	unlock := AcquirePathLock(path)
	defer unlock()

	// Try to acquire the lock again in a separate goroutine, it should block until we release it
	done := make(chan struct{})

	go func() {
		unlock2 := AcquirePathLock(path)
		defer unlock2()

		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("expected second lock acquisition to block, but it succeeded")
	default:
		// Expected to block
	}

	// Now release the first lock and check that the second one can proceed
	unlock()

	select {
	case <-done:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatalf("expected second lock acquisition to succeed after release, but it timed out")
	}
}

// Test that acquiring locks for different paths does not block each other.
func TestAcquirePathLock_ConcurrentLocks(t *testing.T) {
	path := "test/repo/path"

	// Acquire lock in one goroutine
	unlock1 := AcquirePathLock(path)
	defer unlock1()

	// Try to acquire the same lock in another goroutine, it should block until we release it
	done := make(chan struct{})

	go func() {
		unlock2 := AcquirePathLock(path)
		defer unlock2()

		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("expected second lock acquisition to block, but it succeeded")
	default:
		// Expected to block
	}

	unlock1()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatalf("expected second lock acquisition to succeed after release, but it timed out")
	}
}

// Test that acquiring locks for different paths does not block each other.
func TestAcquirePathLock_MultiplePaths(_ *testing.T) {
	path1 := "test/repo/path1"
	path2 := "test/repo/path2"

	unlock1 := AcquirePathLock(path1)
	defer unlock1()

	unlock2 := AcquirePathLock(path2)
	defer unlock2()

	// Both locks should be acquired successfully without blocking each other
}

func TestAcquirePathLock_UnlockTwice(_ *testing.T) {
	path := "test/repo/path"

	unlock := AcquirePathLock(path)

	unlock()

	// Calling unlock again should not cause a panic or error
	unlock()
}
