package stages

import (
	"slices"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/sync/singleflight"

	"github.com/kimdre/doco-cd/internal/git"
)

// gitChangeKey is the key for one repository diff in the cache.
type gitChangeKey struct {
	repository string
	deployed   plumbing.Hash
	latest     plumbing.Hash
}

// GitChangeCache deduplicates immutable repository diffs within one deployment
// batch. A cache belongs to one repository job and must not outlive it.
type GitChangeCache struct {
	mu      sync.RWMutex
	entries map[gitChangeKey][]git.ChangedFile
	group   singleflight.Group
}

// NewGitChangeCache creates a new GitChangeCache.
func NewGitChangeCache() *GitChangeCache {
	return &GitChangeCache{entries: make(map[gitChangeKey][]git.ChangedFile)}
}

// lookup returns the cached diff for one commit pair, if any.
func (c *GitChangeCache) lookup(key gitChangeKey) ([]git.ChangedFile, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cached, ok := c.entries[key]

	return cached, ok
}

// changedFiles returns the diff for one commit pair, computing it at most once
// per cache. Callers receive their own copy so per-stack filtering cannot
// corrupt the shared entry.
func (c *GitChangeCache) changedFiles(
	repository string,
	deployed, latest plumbing.Hash,
	compute func() ([]git.ChangedFile, error),
) ([]git.ChangedFile, error) {
	if c == nil {
		return compute()
	}

	key := gitChangeKey{repository: repository, deployed: deployed, latest: latest}
	if cached, ok := c.lookup(key); ok {
		return slices.Clone(cached), nil
	}

	value, err, _ := c.group.Do(repository+"\x00"+deployed.String()+"\x00"+latest.String(), func() (any, error) {
		if cached, ok := c.lookup(key); ok {
			return cached, nil
		}

		changed, computeErr := compute()
		if computeErr != nil {
			return nil, computeErr
		}

		stored := slices.Clone(changed)

		c.mu.Lock()
		c.entries[key] = stored
		c.mu.Unlock()

		return stored, nil
	})
	if err != nil {
		return nil, err
	}

	shared, ok := value.([]git.ChangedFile)
	if !ok {
		return compute()
	}

	return slices.Clone(shared), nil
}
