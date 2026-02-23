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
}

func TestPerRepoSerialization(t *testing.T) {
	lim := NewDeployerLimiter(2)
	ctx := context.Background()

	order := make([]int, 0)
	mu := sync.Mutex{}

	// Create three jobs for same repo that append their id to order when they run
	wg := sync.WaitGroup{}
	for i := 1; i <= 3; i++ {
		wg.Add(1)

		id := i

		go func() {
			defer wg.Done()

			unlock, err := lim.acquire(ctx, "repo-serial", "ref")
			if err != nil {
				t.Errorf("failed to acquire: %v", err)
				return
			}
			// simulate work
			time.Sleep(20 * time.Millisecond)
			mu.Lock()

			order = append(order, id)
			mu.Unlock()
			unlock()
		}()
	}

	wg.Wait()

	// Expect order to be 1,2,3 (FIFO for same repo)
	for i := 0; i < 3; i++ {
		if order[i] != i+1 {
			t.Fatalf("expected order %v, got %v", []int{1, 2, 3}, order)
		}
	}
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
