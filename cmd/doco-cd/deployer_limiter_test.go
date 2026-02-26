package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestTryAcquire(t *testing.T) {
	lim := NewDeployerLimiter(1)

	unlock, ok := lim.TryAcquire("repoA", "ref1")
	if !ok || unlock == nil {
		t.Fatalf("expected TryAcquire success on empty limiter")
	}

	// second TryAcquire should fail because the per-repo lock is held (and sem capacity 1)
	_, ok2 := lim.TryAcquire("repoA", "ref1")
	if ok2 {
		unlock()
		t.Fatalf("expected TryAcquire to fail when already acquired")
	}

	unlock()

	// now TryAcquire should succeed again
	unlock2, ok3 := lim.TryAcquire("repoA", "ref1")
	if !ok3 || unlock2 == nil {
		t.Fatalf("expected TryAcquire success after release")
	}

	unlock2()
}

func TestDifferentReposParallelism(t *testing.T) {
	// allow max 2 concurrent
	lim := NewDeployerLimiter(2)
	ctx := context.Background()

	start := time.Now()
	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()

		unlock, err := lim.acquire(ctx, "repoA", "ref")
		if err != nil {
			t.Errorf("acquire error: %v", err)
			return
		}
		// hold for 100ms
		time.Sleep(100 * time.Millisecond)
		unlock()
	}()

	// small delay then start repoB
	time.Sleep(5 * time.Millisecond)

	go func() {
		defer wg.Done()

		unlock, err := lim.acquire(ctx, "repoB", "ref")
		if err != nil {
			t.Errorf("acquire error: %v", err)
			return
		}
		// hold for 100ms
		time.Sleep(100 * time.Millisecond)
		unlock()
	}()

	wg.Wait()

	dur := time.Since(start)
	// If they ran sequentially it would be ~200ms; if parallel ~100ms. We allow some slack.
	if dur > 180*time.Millisecond {
		t.Fatalf("expected parallel execution for different repos, took %v", dur)
	}
}

// TestTryAcquire_JoinSameRef verifies a second TryAcquire with the same ref succeeds when global capacity allows.
func TestTryAcquire_JoinSameRef(t *testing.T) {
	lim := NewDeployerLimiter(2)

	unlock1, ok := lim.TryAcquire("repoJoin", "refA")
	if !ok || unlock1 == nil {
		t.Fatalf("expected first TryAcquire to succeed")
	}

	unlock2, ok2 := lim.TryAcquire("repoJoin", "refA")
	if !ok2 || unlock2 == nil {
		unlock1()
		t.Fatalf("expected second TryAcquire to join same ref")
	}

	t.Cleanup(func() {
		unlock1()
		unlock2()
	})

	// Check length of locks for repoJoin is 1 and refCounts for refA is 2
	lim.mu.Lock()

	t.Cleanup(func() {
		lim.mu.Unlock()
	})

	repoEnt, ok := lim.locks["repoJoin"]
	if !ok {
		t.Fatalf("expected repo entry for repoJoin")
	}

	repoEnt.mu.Lock()

	t.Cleanup(func() {
		repoEnt.mu.Unlock()
	})

	if len(repoEnt.refCounts) != 1 {
		t.Fatalf("expected 1 ref in refCounts, got %d", len(repoEnt.refCounts))
	}

	count, ok := repoEnt.refCounts["refA"]

	if !ok || count != 2 {
		t.Fatalf("expected refA count to be 2, got %d", count)
	}
}

// TestTryAcquire_DifferentRef_BlockedThenSucceeds verifies TryAcquire for a different ref fails while active and succeeds after release.
func TestTryAcquire_DifferentRef_BlockedThenSucceeds(t *testing.T) {
	lim := NewDeployerLimiter(1)

	unlock1, ok := lim.TryAcquire("repoEnq", "ref1")
	if !ok || unlock1 == nil {
		t.Fatalf("first TryAcquire failed")
	}

	// different ref should not acquire while ref1 is active
	_, ok2 := lim.TryAcquire("repoEnq", "ref2")
	if ok2 {
		unlock1()
		t.Fatalf("expected TryAcquire for different ref to fail while ref1 is active")
	}

	// release and try again
	unlock1()

	unlock2, ok3 := lim.TryAcquire("repoEnq", "ref2")
	if !ok3 || unlock2 == nil {
		t.Fatalf("expected TryAcquire for ref2 to succeed after release")
	}

	unlock2()
}
