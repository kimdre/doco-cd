package stages

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

func TestGitAncestryCacheCoalescesConcurrentComputations(t *testing.T) {
	t.Parallel()

	cache := NewGitAncestryCache()
	ancestor := plumbing.NewHash("1111111111111111111111111111111111111111")
	descendant := plumbing.NewHash("2222222222222222222222222222222222222222")

	var calls atomic.Int32

	started := make(chan struct{})
	release := make(chan struct{})

	compute := func() (bool, error) {
		if calls.Add(1) == 1 {
			close(started)
		}

		<-release

		return true, nil
	}

	const workers = 16

	var wg sync.WaitGroup

	errs := make(chan error, workers)

	for range workers {
		wg.Go(func() {
			result, err := cache.isAncestor("repo", ancestor, descendant, compute)
			if err == nil && !result {
				err = errors.New("unexpected ancestry result")
			}

			errs <- err
		})
	}

	<-started
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("compute calls = %d, want 1", got)
	}
}

func TestGitAncestryCacheDoesNotCacheErrors(t *testing.T) {
	t.Parallel()

	cache := NewGitAncestryCache()
	ancestor := plumbing.NewHash("1111111111111111111111111111111111111111")
	descendant := plumbing.NewHash("2222222222222222222222222222222222222222")
	wantErr := errors.New("ancestry lookup failed")

	var calls int

	compute := func() (bool, error) {
		calls++
		if calls == 1 {
			return false, wantErr
		}

		return true, nil
	}

	if _, err := cache.isAncestor("repo", ancestor, descendant, compute); !errors.Is(err, wantErr) {
		t.Fatalf("first isAncestor error = %v, want %v", err, wantErr)
	}

	result, err := cache.isAncestor("repo", ancestor, descendant, compute)
	if err != nil {
		t.Fatalf("second isAncestor error = %v", err)
	}

	if !result {
		t.Fatal("expected second isAncestor call to return true")
	}

	if calls != 2 {
		t.Fatalf("compute calls = %d, want 2", calls)
	}
}

// TestGitAncestryCacheSharesResultAcrossStacks is the regression test for the reason this cache
// exists: in a monorepo of many stacks, several stacks are frequently last deployed at the exact
// same commit, so their (deployed, latest) pairs are identical. Only the first stack to ask
// should pay for the ancestry walk; every other stack sharing that pair must reuse the result.
func TestGitAncestryCacheSharesResultAcrossStacks(t *testing.T) {
	t.Parallel()

	cache := NewGitAncestryCache()
	deployed := plumbing.NewHash("1111111111111111111111111111111111111111")
	latest := plumbing.NewHash("2222222222222222222222222222222222222222")

	var calls int

	compute := func() (bool, error) {
		calls++
		return false, nil
	}

	const stacks = 7

	for i := range stacks {
		if _, err := cache.isAncestor("repo", deployed, latest, compute); err != nil {
			t.Fatalf("stack %d: %v", i, err)
		}
	}

	if calls != 1 {
		t.Fatalf("compute calls = %d, want 1 (shared across %d stacks)", calls, stacks)
	}
}

func TestGitAncestryCacheSeparatesCommitPairs(t *testing.T) {
	t.Parallel()

	cache := NewGitAncestryCache()
	first := plumbing.NewHash("1111111111111111111111111111111111111111")
	second := plumbing.NewHash("2222222222222222222222222222222222222222")
	third := plumbing.NewHash("3333333333333333333333333333333333333333")

	var calls int

	compute := func() (bool, error) {
		calls++
		return false, nil
	}

	if _, err := cache.isAncestor("repo", first, second, compute); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.isAncestor("repo", first, third, compute); err != nil {
		t.Fatal(err)
	}

	if calls != 2 {
		t.Fatalf("compute calls = %d, want 2", calls)
	}
}

func TestGitAncestryCacheNilIsPassthrough(t *testing.T) {
	t.Parallel()

	var cache *GitAncestryCache

	var calls int

	compute := func() (bool, error) {
		calls++
		return true, nil
	}

	result, err := cache.isAncestor(
		"repo",
		plumbing.NewHash("1111111111111111111111111111111111111111"),
		plumbing.NewHash("2222222222222222222222222222222222222222"),
		compute,
	)
	if err != nil {
		t.Fatal(err)
	}

	if !result {
		t.Fatal("expected passthrough result to be true")
	}

	if calls != 1 {
		t.Fatalf("compute calls = %d, want 1", calls)
	}
}
