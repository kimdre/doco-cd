package git

import (
	"container/heap"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ChangedFile represents a file that has changed between two commits.
type ChangedFile struct {
	// From represents the file state before the change.
	From diff.File
	// To represents the file state after the change.
	To diff.File
}

// treeDiffFile is a simple implementation of the diff.File interface,
// representing a file in a git tree.
type treeDiffFile struct {
	hash plumbing.Hash
	mode filemode.FileMode
	path string
}

func (f treeDiffFile) Hash() plumbing.Hash     { return f.hash }
func (f treeDiffFile) Mode() filemode.FileMode { return f.mode }
func (f treeDiffFile) Path() string            { return f.path }

// diffFileFromChangeEntry converts a ChangeEntry to a diff.File if it represents a file change.
func diffFileFromChangeEntry(entry object.ChangeEntry) diff.File {
	if entry.Name == "" || !entry.TreeEntry.Mode.IsFile() {
		return nil
	}

	return treeDiffFile{
		hash: entry.TreeEntry.Hash,
		mode: entry.TreeEntry.Mode,
		path: entry.Name,
	}
}

// GetLatestCommit retrieves the last commit hash for a given reference in a repository.
func GetLatestCommit(repo *git.Repository, ref string) (string, error) {
	// Get the reference for the specified ref
	refSet, err := GetReferenceSet(repo, ref)
	if err != nil {
		return plumbing.ZeroHash.String(), err
	}

	// If RemoteRef is empty, it's a commit SHA - return it directly
	if refSet.RemoteRef == "" {
		return string(refSet.LocalRef), nil
	}

	if refSet.RemoteRef.IsTag() {
		hash, err := repo.ResolveRevision(plumbing.Revision(refSet.RemoteRef))
		if err != nil {
			return plumbing.ZeroHash.String(), fmt.Errorf("failed to resolve tag %s: %w", refSet.RemoteRef, err)
		}

		return hash.String(), nil
	}

	return refSet.RemoteHash.String(), nil
}

// GetChangedFilesBetweenCommits retrieves a list of changed files between two commits in a repository.
func GetChangedFilesBetweenCommits(repo *git.Repository, commitHash1, commitHash2 plumbing.Hash) ([]ChangedFile, error) {
	commit1, err := repo.CommitObject(commitHash1)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit From commitHash1 %s: %w", commitHash1, err)
	}

	commit2, err := repo.CommitObject(commitHash2)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit From commitHash2 %s: %w", commitHash2, err)
	}

	tree1, err := commit1.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get tree for commit %s: %w", commitHash1, err)
	}

	tree2, err := commit2.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get tree for commit %s: %w", commitHash2, err)
	}

	// The deployment pipeline only needs changed paths, not textual patches.
	// Tree.Diff retains go-git's rename detection without loading blob contents
	// and generating every file hunk.
	changes, err := tree1.Diff(tree2)
	if err != nil {
		return nil, fmt.Errorf("failed to compare commit trees: %w", err)
	}

	changedFiles := make([]ChangedFile, 0, len(changes))
	for _, change := range changes {
		changedFiles = append(changedFiles, ChangedFile{
			From: diffFileFromChangeEntry(change.From),
			To:   diffFileFromChangeEntry(change.To),
		})
	}

	return changedFiles, nil
}

// IsAncestorCommit reports whether the commit at ancestorHash is an ancestor of, or
// identical to, the commit at descendantHash. It walks history purely from local objects
// (no network round-trip), so it is cheap when the mirror already holds both commits.
//
// An error means the relationship could not be determined — most commonly because one of
// the commits is missing from a shallow mirror, but also on any other lookup or traversal
// failure. Callers must treat that as "unknown", not "false": fail open (proceed) rather
// than silently skipping a deployment on an unproven assumption.
func IsAncestorCommit(repo *git.Repository, ancestorHash, descendantHash plumbing.Hash) (bool, error) {
	ancestor, err := repo.CommitObject(ancestorHash)
	if err != nil {
		return false, fmt.Errorf("failed to get commit %s: %w", ancestorHash, err)
	}

	descendant, err := repo.CommitObject(descendantHash)
	if err != nil {
		return false, fmt.Errorf("failed to get commit %s: %w", descendantHash, err)
	}

	return ancestor.IsAncestor(descendant)
}

// CommitInfo is a single commit exposed to notification templates.
type CommitInfo struct {
	Hash      string
	ShortHash string
	Subject   string
	Author    string
}

// String renders "shortHash subject", the default when a template prints a commit directly.
func (c CommitInfo) String() string {
	return c.ShortHash + " " + c.Subject
}

// maxScannedCommits caps how many commits a changelog walk reads. A path filter only
// returns the commits of one stack, so a stack that was not touched for a long time can
// otherwise pull the whole range through a tree diff before it collects maxCommits.
// It is a variable only so tests can lower it.
var maxScannedCommits = 5000

// newCommitInfo maps a commit to the shape exposed to notification templates.
func newCommitInfo(c *object.Commit) CommitInfo {
	subject := c.Message
	if i := strings.IndexByte(subject, '\n'); i >= 0 {
		subject = subject[:i]
	}

	return CommitInfo{
		Hash:      c.Hash.String(),
		ShortHash: c.Hash.String()[:DefaultShortSHALength],
		Subject:   strings.TrimSpace(subject),
		Author:    c.Author.Name,
	}
}

// walkEntry is a commit queued by rangeWalk.
type walkEntry struct {
	commit *object.Commit
	// excluded marks a commit reachable from an excluded commit.
	excluded bool
	queued   bool
	// seq keeps the order of commits with the same committer time stable.
	seq int
}

// walkQueue is a max-heap of commits by committer time.
type walkQueue []*walkEntry

func (q walkQueue) Len() int { return len(q) }

func (q walkQueue) Less(i, j int) bool {
	ti, tj := q[i].commit.Committer.When, q[j].commit.Committer.When
	if !ti.Equal(tj) {
		return ti.After(tj)
	}

	return q[i].seq < q[j].seq
}

func (q walkQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }

func (q *walkQueue) Push(x any) { *q = append(*q, x.(*walkEntry)) }

func (q *walkQueue) Pop() any {
	old := *q
	e := old[len(old)-1]
	*q = old[:len(old)-1]

	return e
}

// rangeWalk yields the commits reachable from a tip but not from the excluded commits,
// newest first by committer time, like `git rev-list <tip> ^<excluded>`.
//
// The exclusion travels down the history together with the walk. Stopping at the first
// excluded commit instead would lose a merged side branch that is older than the last
// deploy, which is the normal shape of a Renovate or any other pull request merge.
type rangeWalk struct {
	repo    *git.Repository
	queue   walkQueue
	entries map[plumbing.Hash]*walkEntry
	seq     int
	// included counts the queued entries that are not excluded. The range is done when
	// only excluded entries are left.
	included  int
	scanned   int
	truncated bool
}

func newRangeWalk(repo *git.Repository) *rangeWalk {
	return &rangeWalk{repo: repo, entries: make(map[plumbing.Hash]*walkEntry)}
}

func (w *rangeWalk) push(c *object.Commit, excluded bool) {
	if e, ok := w.entries[c.Hash]; ok {
		if excluded && !e.excluded {
			e.excluded = true

			if e.queued {
				w.included--
			}
		}

		return
	}

	e := &walkEntry{commit: c, excluded: excluded, queued: true, seq: w.seq}
	w.seq++
	w.entries[c.Hash] = e
	heap.Push(&w.queue, e)

	if !excluded {
		w.included++
	}
}

// next returns the next commit of the range, nil when the range is done or the scan limit
// was hit.
func (w *rangeWalk) next() (*object.Commit, error) {
	for w.included > 0 {
		if w.scanned >= maxScannedCommits {
			w.truncated = true

			return nil, nil
		}

		e := heap.Pop(&w.queue).(*walkEntry)
		e.queued = false
		w.scanned++

		if !e.excluded {
			w.included--
		}

		parents, err := loadParents(w.repo, e.commit)
		if err != nil {
			return nil, err
		}

		for _, p := range parents {
			w.push(p, e.excluded)
		}

		if !e.excluded {
			return e.commit, nil
		}
	}

	return nil, nil
}

// loadParents returns the parents of c that exist in the repository. A shallow clone ends
// the history at a commit whose parents were not fetched.
func loadParents(repo *git.Repository, c *object.Commit) ([]*object.Commit, error) {
	parents := make([]*object.Commit, 0, len(c.ParentHashes))

	for _, h := range c.ParentHashes {
		p, err := repo.CommitObject(h)
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("failed to read parent %s of %s: %w", h, c.Hash, err)
		}

		parents = append(parents, p)
	}

	return parents, nil
}

// changesPaths reports whether c changed a path pathFilter matches, compared against its
// real parents. A merge only counts when it differs from every parent, the same rule
// `git log -- <paths>` applies: a merge that took the files from one side unchanged did not
// change them, the commits of that side did, and those are part of the range themselves.
func changesPaths(repo *git.Repository, c *object.Commit, pathFilter func(string) bool) (bool, error) {
	tree, err := c.Tree()
	if err != nil {
		return false, fmt.Errorf("failed to read tree of %s: %w", c.Hash, err)
	}

	parents, err := loadParents(repo, c)
	if err != nil {
		return false, err
	}

	if len(parents) == 0 {
		// Parents missing in a shallow clone cannot be compared, the whole tree of the
		// commit would look new.
		if len(c.ParentHashes) > 0 {
			return false, nil
		}

		return treeChangesPaths(nil, tree, pathFilter)
	}

	for _, p := range parents {
		parentTree, err := p.Tree()
		if err != nil {
			return false, fmt.Errorf("failed to read tree of %s: %w", p.Hash, err)
		}

		changed, err := treeChangesPaths(parentTree, tree, pathFilter)
		if err != nil {
			return false, err
		}

		if !changed {
			return false, nil
		}
	}

	return true, nil
}

func treeChangesPaths(from, to *object.Tree, pathFilter func(string) bool) (bool, error) {
	changes, err := object.DiffTree(from, to)
	if err != nil {
		return false, fmt.Errorf("failed to diff trees: %w", err)
	}

	for _, change := range changes {
		if (change.From.Name != "" && pathFilter(change.From.Name)) ||
			(change.To.Name != "" && pathFilter(change.To.Name)) {
			return true, nil
		}
	}

	return false, nil
}

// GetCommitsBetween returns commits reachable from newHash but not from oldHash,
// newest first, capped at maxCommits. After a rebase or force-push, where oldHash is not an
// ancestor of newHash any more, that is the commits since the histories diverged.
//
// A non-nil pathFilter keeps only the commits that changed a path it matches, so a stack
// gets a changelog of its own files instead of everything that happened in the repository.
// Paths are relative to the root of the repository. The cap counts the commits that pass
// the filter, and a nil filter returns the whole range.
func GetCommitsBetween(log *slog.Logger, repo *git.Repository, oldHash, newHash plumbing.Hash, maxCommits int, pathFilter func(string) bool) ([]CommitInfo, error) {
	newCommit, err := repo.CommitObject(newHash)
	if err != nil {
		return nil, fmt.Errorf("failed to read commit log from %s: %w", newHash, err)
	}

	walk := newRangeWalk(repo)

	// An unknown old commit excludes nothing, the range is then the whole history.
	if oldCommit, err := repo.CommitObject(oldHash); err == nil {
		walk.push(oldCommit, true)
	}

	walk.push(newCommit, false)

	commits := make([]CommitInfo, 0, maxCommits)

	for len(commits) < maxCommits {
		c, err := walk.next()
		if err != nil {
			return nil, fmt.Errorf("failed to walk commit log: %w", err)
		}

		if c == nil {
			break
		}

		if pathFilter != nil {
			changed, err := changesPaths(repo, c, pathFilter)
			if err != nil {
				return nil, fmt.Errorf("failed to walk commit log: %w", err)
			}

			if !changed {
				continue
			}
		}

		commits = append(commits, newCommitInfo(c))
	}

	if walk.truncated && len(commits) < maxCommits && log != nil {
		log.Warn("commit changelog stopped at scan limit, older commits are left out",
			slog.Int("scan_limit", maxScannedCommits))
	}

	return commits, nil
}

// GetShortestUniqueCommitHash returns the shortest unique prefix of a commit SHA in the repository.
// Similar to the git command `git rev-parse --short=<length> <commitSHA>`.
func GetShortestUniqueCommitHash(repo *git.Repository, commitSHA string, minLength int) (string, error) {
	if repo == nil {
		return "", errors.New("repository not found")
	}

	if commitSHA == "" {
		return "", errors.New("commit SHA is empty")
	}

	iter, err := repo.Storer.IterEncodedObjects(plumbing.CommitObject)
	if err != nil {
		return "", err
	}
	defer iter.Close()

	var (
		foundCommit    bool
		requiredLength = minLength
	)

	err = iter.ForEach(func(encoded plumbing.EncodedObject) error {
		if encoded == nil {
			return nil
		}

		sha := encoded.Hash().String()
		if sha == commitSHA {
			foundCommit = true

			return nil
		}

		requiredLength = max(requiredLength, sharedPrefixLength(commitSHA, sha)+1)

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("error iterating commits: %w", err)
	}

	if !foundCommit {
		return "", fmt.Errorf("commit SHA %s not found in repository", commitSHA)
	}

	if requiredLength <= len(commitSHA) {
		return commitSHA[:requiredLength], nil
	}

	return "", fmt.Errorf("no unique prefix found for commit SHA %s", commitSHA)
}

// sharedPrefixLength returns the length of the common prefix between two strings.
func sharedPrefixLength(first, second string) int {
	length := min(len(first), len(second))
	for i := range length {
		if first[i] != second[i] {
			return i
		}
	}

	return length
}
