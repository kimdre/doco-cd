package git

import (
	"testing"
	"time"
)

func TestLockForPath(t *testing.T) {
	t.Parallel()

	key := t.Name()

	lock1 := lockForKey(key)
	lock2 := lockForKey(key)

	if lock1 != lock2 {
		t.Fatalf("expected same mutex for the same path, got different instances")
	}
}

func TestAcquirePathLock(t *testing.T) {
	t.Parallel()

	key := t.Name()

	unlock := AcquirePathLock(key)
	defer unlock()

	// Try to acquire the lock again in a separate goroutine, it should block until we release it
	done := make(chan struct{})

	go func() {
		unlock2 := AcquirePathLock(key)
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
	t.Parallel()

	key := t.Name()

	// Acquire lock in one goroutine
	unlock1 := AcquirePathLock(key)
	defer unlock1()

	// Try to acquire the same lock in another goroutine, it should block until we release it
	done := make(chan struct{})

	go func() {
		unlock2 := AcquirePathLock(key)
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
func TestAcquirePathLock_MultiplePaths(t *testing.T) {
	t.Parallel()

	key1 := t.Name() + "1"
	key2 := t.Name() + "2"

	unlock1 := AcquirePathLock(key1)
	defer unlock1()

	unlock2 := AcquirePathLock(key2)
	defer unlock2()

	// Both locks should be acquired successfully without blocking each other
}

func TestAcquirePathLock_UnlockTwice(t *testing.T) {
	t.Parallel()

	key := t.Name()

	unlock := AcquirePathLock(key)

	unlock()

	// Calling unlock again should not cause a panic or error
	unlock()
}
