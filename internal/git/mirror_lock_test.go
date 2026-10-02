package git

import (
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
)

type mirrorLockEvent struct {
	mode         string
	waited, held time.Duration
}

// recordMirrorLocks installs an observer for the duration of the test. The
// observer is process-global, so tests using it must not run in parallel.
func recordMirrorLocks(t *testing.T) func() []mirrorLockEvent {
	t.Helper()

	var (
		mu     sync.Mutex
		events []mirrorLockEvent
	)

	SetMirrorLockObserver(func(mode string, waited, held time.Duration) {
		mu.Lock()
		defer mu.Unlock()

		events = append(events, mirrorLockEvent{mode: mode, waited: waited, held: held})
	})
	t.Cleanup(func() { SetMirrorLockObserver(nil) })

	return func() []mirrorLockEvent {
		mu.Lock()
		defer mu.Unlock()

		return append([]mirrorLockEvent(nil), events...)
	}
}

func TestAcquireObservedMirrorLockReportsWaitAndHoldOnce(t *testing.T) {
	events := recordMirrorLocks(t)

	const (
		wait = 20 * time.Millisecond
		hold = 10 * time.Millisecond
	)

	unlocked := 0
	unlock := acquireObservedMirrorLock(MirrorLockExclusive, "unused", func(string) func() {
		time.Sleep(wait)

		return func() { unlocked++ }
	})

	time.Sleep(hold)
	unlock()
	unlock()

	if unlocked != 1 {
		t.Fatalf("underlying unlock called %d times, want 1", unlocked)
	}

	got := events()
	if len(got) != 1 {
		t.Fatalf("observed %d lock events, want 1", len(got))
	}

	if got[0].mode != MirrorLockExclusive {
		t.Errorf("mode = %q, want %q", got[0].mode, MirrorLockExclusive)
	}

	if got[0].waited < wait {
		t.Errorf("waited = %s, want at least %s", got[0].waited, wait)
	}

	if got[0].held < hold {
		t.Errorf("held = %s, want at least %s", got[0].held, hold)
	}
}

func TestWithMirrorReadReportsContendedSharedLock(t *testing.T) {
	events := recordMirrorLocks(t)

	_, mirrorPath, _ := setupMirrorWithPack(t)

	const hold = 30 * time.Millisecond

	unlockExclusive := AcquireExclusiveMirrorLock(mirrorPath)

	released := make(chan struct{})

	go func() {
		time.Sleep(hold)
		unlockExclusive()
		close(released)
	}()

	if err := WithMirrorRead(mirrorPath, func(*gogit.Repository) error { return nil }); err != nil {
		t.Fatalf("WithMirrorRead: %v", err)
	}

	<-released

	var shared, exclusive *mirrorLockEvent

	for _, event := range events() {
		switch event.mode {
		case MirrorLockShared:
			shared = &event
		case MirrorLockExclusive:
			exclusive = &event
		}
	}

	if exclusive == nil || exclusive.held < hold {
		t.Fatalf("exclusive lock event = %+v, want one held for at least %s", exclusive, hold)
	}

	// The shared read had to wait for most of the exclusive hold.
	if shared == nil || shared.waited < hold/2 {
		t.Fatalf("shared lock event = %+v, want one that waited for the exclusive holder", shared)
	}
}
