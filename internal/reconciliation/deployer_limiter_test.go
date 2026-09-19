package reconciliation

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestTryAcquire(t *testing.T) {
	t.Parallel()

	lim := newTestDeployerLimiter(t, 1)

	unlock, ok := lim.TryAcquire("repoA")
	if !ok || unlock == nil {
		t.Fatalf("expected TryAcquire success on empty limiter")
	}

	// second TryAcquire should fail because the global semaphore (capacity 1) is held
	_, ok2 := lim.TryAcquire("repoA")
	if ok2 {
		unlock()
		t.Fatalf("expected TryAcquire to fail when already acquired")
	}

	unlock()

	// now TryAcquire should succeed again
	unlock2, ok3 := lim.TryAcquire("repoA")
	if !ok3 || unlock2 == nil {
		t.Fatalf("expected TryAcquire success after release")
	}

	unlock2()
}

func TestDifferentReposParallelism(t *testing.T) {
	t.Parallel()

	// allow max 2 concurrent
	lim := newTestDeployerLimiter(t, 2)
	ctx := context.Background()

	start := time.Now()
	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()

		unlock, err := lim.acquire(ctx, "repoA")
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

		unlock, err := lim.acquire(ctx, "repoB")
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

// TestSameRepoDifferentRefsParallelism verifies that two deployments for the
// same repository but different git references now run concurrently
// (subject only to the global semaphore), since immutable per-revision
// artifacts (Phase 2) mean they no longer share a mutable working tree.
func TestSameRepoDifferentRefsParallelism(t *testing.T) {
	t.Parallel()

	// allow max 2 concurrent
	lim := newTestDeployerLimiter(t, 2)
	ctx := context.Background()

	start := time.Now()
	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()

		unlock, err := lim.acquire(ctx, "repoSame")
		if err != nil {
			t.Errorf("acquire error: %v", err)
			return
		}

		time.Sleep(100 * time.Millisecond)
		unlock()
	}()

	time.Sleep(5 * time.Millisecond)

	go func() {
		defer wg.Done()

		unlock, err := lim.acquire(ctx, "repoSame")
		if err != nil {
			t.Errorf("acquire error: %v", err)
			return
		}

		time.Sleep(100 * time.Millisecond)
		unlock()
	}()

	wg.Wait()

	dur := time.Since(start)
	if dur > 180*time.Millisecond {
		t.Fatalf("expected parallel execution for the same repo with different refs, took %v", dur)
	}
}

// TestTryAcquire_JoinSameRepo verifies a second TryAcquire for the same repo
// succeeds when global capacity allows, and that a single per-repo metric
// entry tracks both.
func TestTryAcquire_JoinSameRepo(t *testing.T) {
	t.Parallel()

	lim := newTestDeployerLimiter(t, 2)

	unlock1, ok := lim.TryAcquire("repoJoin")
	if !ok || unlock1 == nil {
		t.Fatalf("expected first TryAcquire to succeed")
	}

	unlock2, ok2 := lim.TryAcquire("repoJoin")
	if !ok2 || unlock2 == nil {
		unlock1()
		t.Fatalf("expected second TryAcquire for the same repo to succeed")
	}

	t.Cleanup(func() {
		unlock1()
		unlock2()
	})

	lim.mu.Lock()

	t.Cleanup(func() {
		lim.mu.Unlock()
	})

	ent, ok := lim.entries["repoJoin"]
	if !ok {
		t.Fatalf("expected repo entry for repoJoin")
	}

	ent.mu.Lock()

	t.Cleanup(func() {
		ent.mu.Unlock()
	})

	if ent.active != 2 {
		t.Fatalf("expected active count to be 2, got %d", ent.active)
	}
}

// TestTryAcquire_ExhaustedSemaphore verifies TryAcquire fails once the
// global semaphore capacity is exhausted, regardless of repo, and succeeds
// again after a release.
func TestTryAcquire_ExhaustedSemaphore(t *testing.T) {
	t.Parallel()

	lim := newTestDeployerLimiter(t, 1)

	unlock1, ok := lim.TryAcquire("repoEnq")
	if !ok || unlock1 == nil {
		t.Fatalf("first TryAcquire failed")
	}

	// capacity is exhausted, even for a different repo
	_, ok2 := lim.TryAcquire("repoOther")
	if ok2 {
		unlock1()
		t.Fatalf("expected TryAcquire to fail while global capacity is exhausted")
	}

	unlock1()

	unlock2, ok3 := lim.TryAcquire("repoOther")
	if !ok3 || unlock2 == nil {
		t.Fatalf("expected TryAcquire to succeed after release")
	}

	unlock2()
}

func TestDeployerLimiterCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	limiter := NewDeployerLimiter(1)
	limiter.Close()
	limiter.Close()

	select {
	case <-limiter.doneChan:
	default:
		t.Fatal("expected limiter cleanup loop to stop")
	}
}
