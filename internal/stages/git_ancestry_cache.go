package stages

import (
	"container/heap"
	"errors"
	"fmt"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
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
//
// The walk visits the newest queued commit first, like git's merge-base search.
// A recently deployed commit is then found after the commits made since, whichever
// parent of a merge it was reached through, instead of after a side branch's
// entire history.
type gitHistory struct {
	mu sync.Mutex
	// started is set once the descendant commit was queued.
	started bool
	// seen holds every commit read from the mirror and known to be reachable
	// from the descendant: the visited commits and the queued parents.
	seen  set.Set[plumbing.Hash]
	queue ancestryQueue
	seq   int
	// visited counts the commits whose parents were read.
	visited int
	// missing holds the parents not found in the mirror, for example beyond a
	// shallow clone's boundary. They are kept out of seen, so a found ancestor
	// can always be read, and the walk cannot prove non-ancestry once any is
	// missing.
	missing set.Set[plumbing.Hash]
}

func newGitHistory() *gitHistory {
	return &gitHistory{
		seen:    set.New[plumbing.Hash](),
		missing: set.New[plumbing.Hash](),
	}
}

// ancestryEntry is one queued commit of an ancestry walk. It keeps only what the
// walk needs: a commit object would pin the storage of the handle that read it,
// pack indexes included, for as long as the job runs.
type ancestryEntry struct {
	hash    plumbing.Hash
	parents []plumbing.Hash
	when    time.Time
	// seq keeps the order of commits with the same committer time stable.
	seq int
}

// ancestryQueue is a max-heap of commits by committer time.
type ancestryQueue []ancestryEntry

func (q ancestryQueue) Len() int { return len(q) }

func (q ancestryQueue) Less(i, j int) bool {
	if !q[i].when.Equal(q[j].when) {
		return q[i].when.After(q[j].when)
	}

	return q[i].seq < q[j].seq
}

func (q ancestryQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }

func (q *ancestryQueue) Push(x any) { *q = append(*q, x.(ancestryEntry)) }

func (q *ancestryQueue) Pop() any {
	old := *q
	e := old[len(old)-1]
	*q = old[:len(old)-1]

	return e
}

// push queues commit and marks it reachable.
func (h *gitHistory) push(commit *object.Commit) {
	h.seen.Add(commit.Hash)
	heap.Push(&h.queue, ancestryEntry{
		hash:    commit.Hash,
		parents: commit.ParentHashes,
		when:    commit.Committer.When,
		seq:     h.seq,
	})
	h.seq++
}

// walkTo continues the walk until ancestor is reached or the history is
// exhausted. The caller must hold h.mu if h is shared.
func (h *gitHistory) walkTo(repo *gogit.Repository, ancestor, descendant plumbing.Hash) (bool, error) {
	if !h.started {
		commit, err := repo.CommitObject(descendant)
		if err != nil {
			return false, fmt.Errorf("failed to get commit %s: %w", descendant, err)
		}

		h.push(commit)
		h.started = true
	}

	for !h.seen.Contains(ancestor) {
		if h.queue.Len() == 0 {
			if len(h.missing) > 0 {
				return false, fmt.Errorf("history of commit %s is incomplete in the mirror", descendant)
			}

			return false, nil
		}

		// Read the parents before dequeuing the commit, so a failed read leaves
		// the walk where it was for the next caller.
		next := h.queue[0]
		parents := make([]*object.Commit, 0, len(next.parents))

		var missing []plumbing.Hash

		for _, hash := range next.parents {
			if h.seen.Contains(hash) || h.missing.Contains(hash) {
				continue
			}

			parent, err := repo.CommitObject(hash)
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				missing = append(missing, hash)
				continue
			} else if err != nil {
				return false, fmt.Errorf("failed to get commit %s: %w", hash, err)
			}

			parents = append(parents, parent)
		}

		heap.Pop(&h.queue)
		h.visited++

		for _, parent := range parents {
			h.push(parent)
		}

		for _, hash := range missing {
			h.missing.Add(hash)
		}
	}

	return true, nil
}

// walkAncestry reports whether ancestor is an ancestor of descendant with a
// walk that no cache keeps. It suits checks no other check can resume, such as
// the reverse stale check: a history kept per deployed commit would only be
// asked about the latest revision again, which the exact pair cache answers,
// while keeping the deployed commit's whole history for the rest of the job.
func walkAncestry(repo *gogit.Repository, ancestor, descendant plumbing.Hash) (bool, error) {
	if _, err := repo.CommitObject(ancestor); err != nil {
		return false, fmt.Errorf("failed to get commit %s: %w", ancestor, err)
	}

	return newGitHistory().walkTo(repo, ancestor, descendant)
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
// the latest commit again when stacks have different deployed revisions. The
// stale check and the project skip check share the walk.
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
		history = newGitHistory()
		c.histories[key] = history
	}
	c.historyMu.Unlock()

	// Seen commits were read from this mirror during the job, so they need no
	// existence check through this stack's handle.
	history.mu.Lock()
	found := history.seen.Contains(ancestor)
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

	return history.walkTo(repo, ancestor, descendant)
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
