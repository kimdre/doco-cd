package stages

import (
	"fmt"
	"sync"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/sync/singleflight"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/git"
)

// gitAncestryKey identifies one ancestry check (is ancestor an ancestor of descendant?)
// in the cache.
type gitAncestryKey struct {
	repository string
	ancestor   plumbing.Hash
	descendant plumbing.Hash
}

// GitAncestryCache deduplicates exact ancestry checks and shares a forward
// history walk between stacks deployed at different commits of the same
// revision. A cache belongs to one repository job and must not outlive it.
type GitAncestryCache struct {
	mu      sync.RWMutex
	entries map[gitAncestryKey]bool
	group   singleflight.Group

	historyMu sync.Mutex
	histories map[gitHistoryKey]*gitHistory
}

// gitHistoryKey identifies one ancestry walk
// (from descendant to its ancestors) in the cache.
type gitHistoryKey struct {
	repository string
	descendant plumbing.Hash
}

// gitHistory is a forward ancestry walk from one descendant commit to its ancestors.
type gitHistory struct {
	mu      sync.Mutex
	visited set.Set[plumbing.Hash]
	pending []plumbing.Hash
}

// NewGitAncestryCache creates a new GitAncestryCache.
func NewGitAncestryCache() *GitAncestryCache {
	return &GitAncestryCache{
		entries:   make(map[gitAncestryKey]bool),
		histories: make(map[gitHistoryKey]*gitHistory),
	}
}

// isAncestorFromHistory incrementally walks the latest commit's ancestry.
// Each stack resumes where the previous one stopped rather than starting at
// the latest commit again when stacks have different deployed revisions.
func (c *GitAncestryCache) isAncestorFromHistory(
	repo *gogit.Repository, repository string, ancestor, descendant plumbing.Hash,
) (bool, error) {
	if c == nil {
		return git.IsAncestorCommit(repo, ancestor, descendant)
	}

	key := gitHistoryKey{repository: repository, descendant: descendant}

	c.historyMu.Lock()

	history := c.histories[key]
	if history == nil {
		history = &gitHistory{
			visited: set.New[plumbing.Hash](),
			pending: []plumbing.Hash{descendant},
		}
		c.histories[key] = history
	}
	c.historyMu.Unlock()

	// Visited commits were read from this mirror during the job, so they need
	// no existence check through this stack's handle.
	history.mu.Lock()
	found := history.visited.Contains(ancestor)
	history.mu.Unlock()

	if found {
		return true, nil
	}

	// A fresh handle loads every pack index on its first object read. Do that
	// outside the shared walk so stacks do not warm their handles one by one.
	if _, err := repo.CommitObject(ancestor); err != nil {
		return false, fmt.Errorf("failed to get commit %s: %w", ancestor, err)
	}

	history.mu.Lock()
	defer history.mu.Unlock()

	if history.visited.Contains(ancestor) {
		return true, nil
	}

	for len(history.pending) > 0 {
		index := len(history.pending) - 1

		hash := history.pending[index]
		if history.visited.Contains(hash) {
			history.pending = history.pending[:index]
			continue
		}

		commit, err := repo.CommitObject(hash)
		if err != nil {
			return false, fmt.Errorf("failed to get commit %s: %w", hash, err)
		}

		history.pending = history.pending[:index]
		history.visited.Add(hash)
		history.pending = append(history.pending, commit.ParentHashes...)

		if hash == ancestor {
			return true, nil
		}
	}

	return false, nil
}

// lookup returns the cached result for one ancestry pair, if any.
func (c *GitAncestryCache) lookup(key gitAncestryKey) (bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cached, ok := c.entries[key]

	return cached, ok
}

// isAncestor returns whether ancestor is an ancestor of descendant, computing it at most once
// per cache for each exact (ancestor, descendant) pair. Errors are never cached: a transient
// lookup/traversal failure (e.g. a shallow mirror momentarily missing a commit) must not stick
// around for later callers sharing the same pair.
func (c *GitAncestryCache) isAncestor(
	repository string,
	ancestor, descendant plumbing.Hash,
	compute func() (bool, error),
) (bool, error) {
	if c == nil {
		return compute()
	}

	key := gitAncestryKey{repository: repository, ancestor: ancestor, descendant: descendant}
	if cached, ok := c.lookup(key); ok {
		return cached, nil
	}

	value, err, _ := c.group.Do(repository+"\x00"+ancestor.String()+"\x00"+descendant.String(), func() (any, error) {
		if cached, ok := c.lookup(key); ok {
			return cached, nil
		}

		result, computeErr := compute()
		if computeErr != nil {
			return nil, computeErr
		}

		c.mu.Lock()
		c.entries[key] = result
		c.mu.Unlock()

		return result, nil
	})
	if err != nil {
		return false, err
	}

	result, ok := value.(bool)
	if !ok {
		return compute()
	}

	return result, nil
}
