package stages

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/kimdre/doco-cd/internal/git"
)

func TestGitAncestryCacheSharesHistoryAcrossDifferentDeployedCommits(t *testing.T) {
	t.Parallel()

	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatal(err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	hashes := commitN(t, wt, 20, 0)
	cache := NewGitAncestryCache()
	latest := hashes[len(hashes)-1]

	for _, deployed := range []plumbing.Hash{hashes[15], hashes[7], hashes[12], hashes[0]} {
		got, err := cache.isAncestorFromHistory(repo, "repo", deployed, latest)
		if err != nil || !got {
			t.Fatalf("deployed %s: ancestor = %t, err = %v", deployed, got, err)
		}

		history := cache.histories[gitHistoryKey{"repo", latest}]
		if !history.visited.Contains(deployed) {
			t.Fatalf("history did not retain deployed commit %s", deployed)
		}
	}

	for _, deployed := range hashes {
		got, err := cache.isAncestorFromHistory(repo, "repo", deployed, latest)
		if err != nil || !got {
			t.Fatalf("deployed %s: ancestor = %t, err = %v", deployed, got, err)
		}
	}

	if got := len(cache.histories[gitHistoryKey{"repo", latest}].visited); got != len(hashes) {
		t.Fatalf("visited %d commits, want %d (each commit walked once)", got, len(hashes))
	}
}

func TestGitAncestryCacheVisitedCommitDoesNotReadHandle(t *testing.T) {
	t.Parallel()

	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatal(err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	hashes := commitN(t, wt, 5, 0)
	cache := NewGitAncestryCache()
	latest := hashes[len(hashes)-1]

	if got, err := cache.isAncestorFromHistory(repo, "repo", hashes[0], latest); err != nil || !got {
		t.Fatalf("initial walk: ancestor = %t, err = %v", got, err)
	}

	// Each stack opens its own mirror handle. A commit already visited by the
	// shared walk must be answered without reading through the new handle.
	empty, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatal(err)
	}

	for _, deployed := range hashes {
		if got, err := cache.isAncestorFromHistory(empty, "repo", deployed, latest); err != nil || !got {
			t.Fatalf("visited %s: ancestor = %t, err = %v", deployed, got, err)
		}
	}

	if _, err := cache.isAncestorFromHistory(empty, "repo", plumbing.NewHash("1234"), latest); err == nil {
		t.Fatal("unvisited commit missing from the handle returned no error")
	}
}

func TestGitAncestryCacheHistoryDivergenceAndMissingCommit(t *testing.T) {
	t.Parallel()

	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatal(err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	base := commitN(t, wt, 1, 0)[0]

	first := commitN(t, wt, 1, 1)[0]
	if err := wt.Checkout(&gogit.CheckoutOptions{Hash: base}); err != nil {
		t.Fatal(err)
	}

	second := commitN(t, wt, 1, 2)[0]
	cache := NewGitAncestryCache()

	for _, tc := range []struct {
		ancestor   plumbing.Hash
		descendant plumbing.Hash
	}{
		{first, second},
		{base, second},
		{second, first},
		{second, second},
	} {
		want, err := git.IsAncestorCommit(repo, tc.ancestor, tc.descendant)
		if err != nil {
			t.Fatal(err)
		}

		got, err := cache.isAncestorFromHistory(repo, "repo", tc.ancestor, tc.descendant)
		if err != nil || got != want {
			t.Fatalf("ancestor %s of %s: got %t, want %t, err %v", tc.ancestor, tc.descendant, got, want, err)
		}
	}

	missing := plumbing.NewHash("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if _, err := cache.isAncestorFromHistory(repo, "repo", missing, second); err == nil {
		t.Fatal("missing commit must not be treated as unrelated history")
	}

	got, err := cache.isAncestorFromHistory(repo, "repo", base, second)
	if err != nil || !got {
		t.Fatalf("history after missing commit: got %t, err %v", got, err)
	}
}

func TestGitAncestryCacheConcurrentDifferentDeployedCommits(t *testing.T) {
	t.Parallel()

	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatal(err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	hashes := commitN(t, wt, 30, 0)
	cache := NewGitAncestryCache()
	latest := hashes[len(hashes)-1]
	results := make(chan error, len(hashes))

	var wg sync.WaitGroup
	for _, deployed := range hashes {
		wg.Go(func() {
			got, err := cache.isAncestorFromHistory(repo, "repo", deployed, latest)
			if err == nil && !got {
				err = errors.New("reachable commit not found")
			}

			results <- err
		})
	}

	wg.Wait()
	close(results)

	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}

	if got := len(cache.histories[gitHistoryKey{"repo", latest}].visited); got != len(hashes) {
		t.Fatalf("visited %d commits, want %d", got, len(hashes))
	}
}

func BenchmarkGitAncestryAcrossStacks(b *testing.B) {
	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		b.Fatal(err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		b.Fatal(err)
	}

	hashes := make([]plumbing.Hash, 200)
	for i := range hashes {
		hash, err := wt.Commit("commit", &gogit.CommitOptions{
			AllowEmptyCommits: true,
			Author: &object.Signature{
				Name: "test", Email: "test@example.com", When: time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC),
			},
		})
		if err != nil {
			b.Fatal(err)
		}

		hashes[i] = hash
	}

	latest := hashes[len(hashes)-1]
	deployed := hashes[:40]

	b.Run("independent walks", func(b *testing.B) {
		for range b.N {
			for _, hash := range deployed {
				if _, err := git.IsAncestorCommit(repo, hash, latest); err != nil {
					b.Fatal(err)
				}
			}
		}
	})

	b.Run("shared history", func(b *testing.B) {
		for range b.N {
			cache := NewGitAncestryCache()
			for _, hash := range deployed {
				if _, err := cache.isAncestorFromHistory(repo, "repo", hash, latest); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
}

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
