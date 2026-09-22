package stages

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/git"
)

func TestGitChangeCacheCoalescesConcurrentComputations(t *testing.T) {
	t.Parallel()

	cache := NewGitChangeCache()
	deployed := plumbing.NewHash("1111111111111111111111111111111111111111")
	latest := plumbing.NewHash("2222222222222222222222222222222222222222")

	var calls atomic.Int32

	started := make(chan struct{})
	release := make(chan struct{})

	compute := func() ([]git.ChangedFile, error) {
		if calls.Add(1) == 1 {
			close(started)
		}

		<-release

		return []git.ChangedFile{{}}, nil
	}

	const workers = 16

	var wg sync.WaitGroup

	errs := make(chan error, workers)

	for range workers {
		wg.Go(func() {
			changed, err := cache.changedFiles("repo", deployed, latest, compute)
			if err == nil && len(changed) != 1 {
				err = errors.New("unexpected changed-file count")
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

func TestGitChangeCacheDoesNotCacheErrors(t *testing.T) {
	t.Parallel()

	cache := NewGitChangeCache()
	deployed := plumbing.NewHash("1111111111111111111111111111111111111111")
	latest := plumbing.NewHash("2222222222222222222222222222222222222222")
	wantErr := errors.New("diff failed")

	var calls int

	compute := func() ([]git.ChangedFile, error) {
		calls++
		if calls == 1 {
			return nil, wantErr
		}

		return []git.ChangedFile{{}}, nil
	}

	if _, err := cache.changedFiles("repo", deployed, latest, compute); !errors.Is(err, wantErr) {
		t.Fatalf("first changedFiles error = %v, want %v", err, wantErr)
	}

	if _, err := cache.changedFiles("repo", deployed, latest, compute); err != nil {
		t.Fatalf("second changedFiles error = %v", err)
	}

	if calls != 2 {
		t.Fatalf("compute calls = %d, want 2", calls)
	}
}

func TestGitChangeCacheSeparatesCommitPairs(t *testing.T) {
	t.Parallel()

	cache := NewGitChangeCache()
	first := plumbing.NewHash("1111111111111111111111111111111111111111")
	second := plumbing.NewHash("2222222222222222222222222222222222222222")
	third := plumbing.NewHash("3333333333333333333333333333333333333333")

	var calls int

	compute := func() ([]git.ChangedFile, error) {
		calls++
		return []git.ChangedFile{{}}, nil
	}

	if _, err := cache.changedFiles("repo", first, second, compute); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.changedFiles("repo", first, third, compute); err != nil {
		t.Fatal(err)
	}

	if calls != 2 {
		t.Fatalf("compute calls = %d, want 2", calls)
	}
}

func TestGitChangeCacheReturnsIndependentSlices(t *testing.T) {
	t.Parallel()

	cache := NewGitChangeCache()
	deployed := plumbing.NewHash("1111111111111111111111111111111111111111")
	latest := plumbing.NewHash("2222222222222222222222222222222222222222")

	compute := func() ([]git.ChangedFile, error) {
		return []git.ChangedFile{{}, {}}, nil
	}

	first, err := cache.changedFiles("repo", deployed, latest, compute)
	if err != nil {
		t.Fatal(err)
	}

	first = first[:1]
	if len(first) != 1 {
		t.Fatalf("locally shortened changed-file count = %d, want 1", len(first))
	}

	second, err := cache.changedFiles("repo", deployed, latest, compute)
	if err != nil {
		t.Fatal(err)
	}

	if len(second) != 2 {
		t.Fatalf("cached changed-file count = %d, want 2", len(second))
	}
}
