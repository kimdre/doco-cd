package stages

import (
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/sync/singleflight"
)

// gitAncestryKey identifies one ancestry check (is ancestor an ancestor of descendant?)
// in the cache.
type gitAncestryKey struct {
	repository string
	ancestor   plumbing.Hash
	descendant plumbing.Hash
}

// GitAncestryCache deduplicates ancestry checks (git.IsAncestorCommit) within one deployment
// batch. In a monorepo of multiple stacks, many stacks are typically last deployed at the same
// commit, so several stacks in one job resolve to the exact same (deployed, latest) commit pair.
// Each ancestry check walks real commit history and can be expensive on hosts with slow object
// I/O, so caching the boolean result per exact pair lets stacks that share a pair reuse one
// walk instead of repeating it. A cache belongs to one repository job and must not outlive it.
type GitAncestryCache struct {
	mu      sync.RWMutex
	entries map[gitAncestryKey]bool
	group   singleflight.Group
}

// NewGitAncestryCache creates a new GitAncestryCache.
func NewGitAncestryCache() *GitAncestryCache {
	return &GitAncestryCache{entries: make(map[gitAncestryKey]bool)}
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
