package lock

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestRepoLockLockContextHonorsCancellation(t *testing.T) {
	t.Parallel()

	lock := &RepoLock{}
	if !lock.TryLock("holder") {
		t.Fatal("failed to acquire test lock")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if lock.LockContext(ctx, "waiter") {
		t.Fatal("acquired lock after cancellation")
	}

	lock.Unlock()

	if !lock.TryLock("next") {
		t.Fatal("cancelled waiter retained the lock")
	}
	lock.Unlock()
}

func TestRepoLockLockContextPreservesWaiterOrder(t *testing.T) {
	t.Parallel()

	lock := &RepoLock{}
	if !lock.TryLock("holder") {
		t.Fatal("failed to acquire test lock")
	}

	acquired := make(chan string, 2)
	releases := map[string]chan struct{}{
		"first":  make(chan struct{}),
		"second": make(chan struct{}),
	}

	for index, jobID := range []string{"first", "second"} {
		go func() {
			if lock.LockContext(t.Context(), jobID) {
				acquired <- jobID

				<-releases[jobID]
				lock.Unlock()
			}
		}()

		deadline := time.Now().Add(time.Second)

		for {
			lock.mu.Lock()
			waiterCount := len(lock.waiters)
			lock.mu.Unlock()

			if waiterCount == index+1 {
				break
			}

			if time.Now().After(deadline) {
				t.Fatal("waiter was not queued")
			}

			time.Sleep(time.Millisecond)
		}
	}

	lock.Unlock()

	for _, want := range []string{"first", "second"} {
		select {
		case got := <-acquired:
			if got != want {
				t.Fatalf("acquisition order = %q, want %q", got, want)
			}

			close(releases[want])
		case <-time.After(time.Second):
			t.Fatalf("waiter %q did not acquire lock", want)
		}
	}
}

func TestRepoLockLockContextCancellationSkipsQueuedWaiter(t *testing.T) {
	t.Parallel()

	lock := &RepoLock{}
	if !lock.TryLock("holder") {
		t.Fatal("failed to acquire test lock")
	}

	cancelledCtx, cancel := context.WithCancel(t.Context())
	firstDone := make(chan bool, 1)

	go func() {
		firstDone <- lock.LockContext(cancelledCtx, "cancelled")
	}()

	secondAcquired := make(chan bool, 1)
	go func() {
		secondAcquired <- lock.LockContext(t.Context(), "second")
	}()

	deadline := time.Now().Add(time.Second)

	for {
		lock.mu.Lock()
		waiterCount := len(lock.waiters)
		lock.mu.Unlock()

		if waiterCount == 2 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("waiters were not queued")
		}

		time.Sleep(time.Millisecond)
	}

	cancel()

	if acquired := <-firstDone; acquired {
		t.Fatal("cancelled waiter acquired lock")
	}

	lock.Unlock()

	select {
	case acquired := <-secondAcquired:
		if !acquired {
			t.Fatal("second waiter did not acquire lock")
		}
	case <-time.After(time.Second):
		t.Fatal("second waiter did not acquire lock")
	}

	lock.Unlock()
}

// TestRepoLockCancelledWaiterAfterGrantReleasesOwnership covers the
// cancellation-races-with-handoff path of LockContext: the waiter's context is
// cancelled, but Unlock grants it ownership before it can dequeue itself, so
// removeQueuedWaiter returns false and LockContext must run the compensating
// Unlock. Without that compensation the lock is leaked forever.
//
// The interleaving is forced by holding lock.mu while cancelling: the waiter
// deterministically takes the ctx.Done() select case (granted is not yet
// closed) and then blocks on mu inside removeQueuedWaiter, after which the
// test performs the grant before releasing mu.
func TestRepoLockCancelledWaiterAfterGrantReleasesOwnership(t *testing.T) {
	t.Parallel()

	for attempt := range 10 {
		lock := &RepoLock{}
		if !lock.TryLock("holder") {
			t.Fatal("failed to acquire test lock")
		}

		ctx, cancel := context.WithCancel(t.Context())
		got := make(chan bool, 1)

		go func() {
			got <- lock.LockContext(ctx, "cancelled")
		}()

		deadline := time.Now().Add(time.Second)

		for {
			lock.mu.Lock()
			waiterCount := len(lock.waiters)
			lock.mu.Unlock()

			if waiterCount == 1 {
				break
			}

			if time.Now().After(deadline) {
				t.Fatal("waiter was not queued")
			}

			time.Sleep(time.Millisecond)
		}

		lock.mu.Lock()
		cancel()

		// Give the waiter time to observe ctx.Done() and block on mu inside
		// removeQueuedWaiter before the grant happens.
		time.Sleep(50 * time.Millisecond)

		// Grant ownership to the cancelled waiter while still holding mu,
		// mirroring what Unlock does when it hands off to the queue head.
		waiter := lock.waiters[0]
		lock.waiters = lock.waiters[1:]
		lock.holder = waiter.jobID
		close(waiter.granted)
		lock.mu.Unlock()

		var acquired bool

		select {
		case acquired = <-got:
		case <-time.After(time.Second):
			t.Fatal("cancelled waiter did not return")
		}

		if acquired {
			// The scheduler outran the 50ms window and the waiter consumed the
			// grant instead of ctx.Done(); clean up and retry the interleaving.
			lock.Unlock()

			continue
		}

		// LockContext returned false after receiving ownership, so its
		// compensating Unlock must have released the lock.
		if !lock.TryLock("next") {
			t.Fatalf("attempt %d: lock was leaked after cancelled handoff", attempt)
		}

		lock.Unlock()

		return
	}

	t.Fatal("cancellation-after-grant interleaving was never observed")
}

// TestRepoLockUnlockOfUnlockedPanics pins the sync.Mutex-style misuse
// semantics: releasing a lock that is not held is a programmer error and must
// panic instead of silently corrupting the waiter queue.
func TestRepoLockUnlockOfUnlockedPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("Unlock of an unlocked RepoLock did not panic")
		}
	}()

	(&RepoLock{}).Unlock()
}

// reset helper to isolate tests.
func resetRepoLocks(t *testing.T) {
	t.Helper()

	repoLocks = sync.Map{}
}

func TestGetRepoLock_SameAndDifferentRepos(t *testing.T) {
	t.Cleanup(func() { resetRepoLocks(t) })

	repoName := t.Name()

	l1 := GetRepoLock(repoName + "-1")
	l2 := GetRepoLock(repoName + "-1")

	if l1 != l2 {
		t.Fatalf("expected same lock instance for same repo")
	}

	l3 := GetRepoLock(repoName + "-2")
	if l1 == l3 {
		t.Fatalf("expected different lock instances for different repos")
	}
}

func TestRepoLock_TryLockSequence_SingleRepo(t *testing.T) {
	t.Cleanup(func() { resetRepoLocks(t) })

	repoName := t.Name()

	l := GetRepoLock(repoName)

	if ok := l.TryLock("job-1"); !ok {
		t.Fatalf("expected first TryLock to succeed")
	}

	if holder := l.Holder(); holder != "job-1" {
		t.Fatalf("unexpected holder after first lock: got %q want %q", holder, "job-1")
	}

	if ok := l.TryLock("job-2"); ok {
		t.Fatalf("expected second TryLock to fail while locked")
	}

	if holder := l.Holder(); holder != "job-1" {
		t.Fatalf("holder changed unexpectedly: got %q want %q", holder, "job-1")
	}

	l.Unlock()

	if holder := l.Holder(); holder != "" {
		t.Fatalf("holder should be empty after Unlock, got %q", holder)
	}

	if ok := l.TryLock("job-2"); !ok {
		t.Fatalf("expected TryLock to succeed after Unlock")
	}

	if holder := l.Holder(); holder != "job-2" {
		t.Fatalf("unexpected holder after relock: got %q want %q", holder, "job-2")
	}

	l.Unlock()
}

func TestRepoLock_ConcurrentTryLock_SameRepo(t *testing.T) {
	t.Cleanup(func() { resetRepoLocks(t) })

	repoName := t.Name()

	const goroutines = 20

	l := GetRepoLock(repoName)

	var wg sync.WaitGroup
	wg.Add(goroutines)

	var (
		mu      sync.Mutex
		winners []string
	)

	for i := range goroutines {
		jobID := "job-" + strconv.Itoa(i)
		go func(id string) {
			defer wg.Done()

			if l.TryLock(id) {
				mu.Lock()

				winners = append(winners, id)

				mu.Unlock()
				// do not unlock here to simulate webhook immediate return on success
			}
		}(jobID)
	}

	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("expected exactly one winner, got %d (%v)", len(winners), winners)
	}

	if holder := l.Holder(); holder != winners[0] {
		t.Fatalf("holder mismatch: got %q want %q", holder, winners[0])
	}

	// After unlock, another job should be able to acquire the lock
	l.Unlock()

	if ok := l.TryLock("job-next"); !ok {
		t.Fatalf("expected TryLock to succeed after Unlock")
	}

	if holder := l.Holder(); holder != "job-next" {
		t.Fatalf("unexpected holder after next lock: got %q want %q", holder, "job-next")
	}

	l.Unlock()
}

func TestRepoLock_IndependentRepos(t *testing.T) {
	t.Cleanup(func() { resetRepoLocks(t) })

	la := GetRepoLock(t.Name() + "-A")
	lb := GetRepoLock(t.Name() + "-B")

	if !la.TryLock("job-A1") {
		t.Fatalf("repoA first lock should succeed")
	}

	if !lb.TryLock("job-B1") {
		t.Fatalf("repoB first lock should succeed")
	}

	if la.Holder() != "job-A1" {
		t.Fatalf("repoA holder mismatch: got %q want %q", la.Holder(), "job-A1")
	}

	if lb.Holder() != "job-B1" {
		t.Fatalf("repoB holder mismatch: got %q want %q", lb.Holder(), "job-B1")
	}

	// Second lock attempts should fail independently
	if la.TryLock("job-A2") {
		t.Fatalf("repoA second lock should fail while locked")
	}

	if lb.TryLock("job-B2") {
		t.Fatalf("repoB second lock should fail while locked")
	}

	// Unlock A and relock, B remains unaffected
	la.Unlock()

	if !la.TryLock("job-A2") {
		t.Fatalf("repoA relock should succeed after unlock")
	}

	if la.Holder() != "job-A2" {
		t.Fatalf("repoA holder mismatch after relock: got %q want %q", la.Holder(), "job-A2")
	}

	if lb.Holder() != "job-B1" {
		t.Fatalf("repoB holder should be unchanged: got %q want %q", lb.Holder(), "job-B1")
	}

	la.Unlock()
	lb.Unlock()
}

func TestLockStack_MutualExclusion_SameStack(t *testing.T) {
	// No resetStackLocks here: goroutines may outlive the test cleanup window,
	// and unique t.Name() keys already guarantee isolation between tests.
	stackName := t.Name()

	ready := make(chan struct{})
	release := make(chan struct{})
	unlocked := make(chan struct{})
	acquired := make(chan struct{})

	go func() {
		LockStack(stackName)
		close(ready)
		<-release
		UnlockStack(stackName)
		close(unlocked)
	}()

	<-ready

	go func() {
		LockStack(stackName)
		close(acquired)
		UnlockStack(stackName)
	}()

	select {
	case <-acquired:
		close(release)
		t.Fatalf("expected second lock acquisition to block while first holder is active")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for blocked lock acquisition")
	}

	<-unlocked
}

func TestLockStack_DifferentStacksDontBlock(t *testing.T) {
	t.Parallel()

	stackA := t.Name() + "-A"
	stackB := t.Name() + "-B"

	LockStack(stackA)
	defer UnlockStack(stackA)

	acquired := make(chan struct{})

	go func() {
		LockStack(stackB)
		close(acquired)
		UnlockStack(stackB)
	}()

	select {
	case <-acquired:
		// correct: stack B did not block on stack A's lock
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("LockStack for a different stack should not block")
	}
}

func TestLockStack_ReacquireAfterUnlock(t *testing.T) {
	t.Parallel()

	stackName := t.Name()

	LockStack(stackName)
	UnlockStack(stackName)

	done := make(chan struct{})

	go func() {
		LockStack(stackName)
		UnlockStack(stackName)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected lock reacquisition to succeed after unlock")
	}
}

func TestLockStack_EmptyNameIsNoOp(t *testing.T) {
	t.Parallel()

	// Neither call should panic or block.
	LockStack("")
	UnlockStack("")
}

// TestStackKey verifies that the default context keeps the bare stack name, which is what
// the job scheduler and the certificate rotation watcher lock on, while named contexts are
// namespaced so same-named stacks on different Docker hosts don't block each other.
func TestStackKey(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		contextName string
		stackName   string
		want        string
	}{
		{name: "default context keeps bare stack name", contextName: "", stackName: "telegraf", want: "telegraf"},
		{name: "blank context is treated as default", contextName: "   ", stackName: "telegraf", want: "telegraf"},
		{name: "explicit default context keeps bare stack name", contextName: "default", stackName: "telegraf", want: "telegraf"},
		{name: "default context is case insensitive", contextName: " DEFAULT ", stackName: "telegraf", want: "telegraf"},
		{name: "named context is namespaced", contextName: "docker01", stackName: "telegraf", want: "docker01/telegraf"},
		{name: "context is trimmed", contextName: " docker02 ", stackName: "telegraf", want: "docker02/telegraf"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := StackKey(tc.contextName, tc.stackName); got != tc.want {
				t.Fatalf("StackKey(%q, %q) = %q, want %q", tc.contextName, tc.stackName, got, tc.want)
			}
		})
	}

	if StackKey("docker01", "telegraf") == StackKey("docker02", "telegraf") {
		t.Fatal("expected same-named stacks on different contexts to produce distinct lock keys")
	}
}

// TestLockStack_SameStackDifferentContextsDontBlock ensures a deployment of a stack on one
// Docker context does not serialize behind a same-named stack on another context.
func TestLockStack_SameStackDifferentContextsDontBlock(t *testing.T) {
	t.Parallel()

	stackName := t.Name()
	keyA := StackKey("docker01", stackName)
	keyB := StackKey("docker02", stackName)

	LockStack(keyA)
	defer UnlockStack(keyA)

	acquired := make(chan struct{})

	go func() {
		LockStack(keyB)
		close(acquired)
		UnlockStack(keyB)
	}()

	select {
	case <-acquired:
		// correct: the same stack name on another context did not block
	case <-time.After(200 * time.Millisecond):
		t.Fatal("LockStack for the same stack name on a different context should not block")
	}
}
