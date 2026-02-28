package main

import (
	"strconv"
	"sync"
	"testing"
)

func TestShouldForceDeploy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		commits  []string
		expected []bool
	}{
		{
			name:     "No loop detected",
			commits:  []string{"commit1", "commit2", "commit3"},
			expected: []bool{false, false, false},
		},
		{
			name:     "Loop detected after 3 same commits",
			commits:  []string{"commitX", "commitX", "commitX", "commitY"},
			expected: []bool{false, false, true, false},
		},
		{
			name:     "Multiple stacks with independent loops",
			commits:  []string{"commitA", "commitA", "commitB", "commitB", "commitB"},
			expected: []bool{false, false, false, false, true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stackName := t.Name()

			// Reset the tracker for each test case
			deploymentLoopTracker.Lock()
			deploymentLoopTracker.loops = make(map[string]struct {
				lastCommit string
				count      uint
			})
			deploymentLoopTracker.Unlock()

			for i, commit := range tt.commits {
				result := shouldForceDeploy(stackName, commit, 3)
				if result != tt.expected[i] {
					t.Errorf("shouldForceDeploy(%s, %s) = %v; want %v", stackName, commit, result, tt.expected[i])
				}
			}
		})
	}
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

	for i := 0; i < goroutines; i++ {
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
