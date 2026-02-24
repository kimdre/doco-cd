package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestTryAcquireBasic(t *testing.T) {
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
