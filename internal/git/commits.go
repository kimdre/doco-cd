package git

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// ChangedFile represents a file that has changed between two commits.
type ChangedFile struct {
	// From represents the file state before the change.
	From diff.File
	// To represents the file state after the change.
	To diff.File
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

	// Create a patch between the two commits
	patch, err := commit1.Patch(commit2)
	if err != nil {
		return nil, fmt.Errorf("failed to create patch: %w", err)
	}

	changedFiles := make([]ChangedFile, 0, len(patch.FilePatches()))
	for _, file := range patch.FilePatches() {
		from, to := file.Files()
		changedFiles = append(changedFiles, ChangedFile{From: from, To: to})
	}

	return changedFiles, nil
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

// commitBoundary returns the hashes at which GetCommitsBetween should stop walking:
// oldHash plus the merge-base of old and new. On a normal fast-forward the merge-base
// is oldHash itself; after a rebase/force-push it is the point where the histories
// diverged, so only the genuinely new commits are returned instead of the whole branch.
func commitBoundary(repo *git.Repository, oldHash, newHash plumbing.Hash) map[plumbing.Hash]struct{} {
	boundary := map[plumbing.Hash]struct{}{oldHash: {}}

	newCommit, err := repo.CommitObject(newHash)
	if err != nil {
		return boundary
	}

	oldCommit, err := repo.CommitObject(oldHash)
	if err != nil {
		return boundary
	}

	bases, err := newCommit.MergeBase(oldCommit)
	if err != nil {
		return boundary
	}

	for _, b := range bases {
		boundary[b.Hash] = struct{}{}
	}

	return boundary
}

// errStopWalk ends a commit log walk early.
var errStopWalk = errors.New("stop walk")

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

// boundedCommitIter ends a commit walk at the first boundary commit, and after
// maxScannedCommits commits at the latest.
//
// The boundary commit is still passed on, because object.NewCommitPathIterFromIter diffs
// every commit against the next commit of the source iterator. Without it the oldest
// commit of the range would be diffed against an empty tree and always look like a change.
// Callers drop it by the same boundary check.
type boundedCommitIter struct {
	src      object.CommitIter
	boundary map[plumbing.Hash]struct{}
	scanned  int
	done     bool
	// truncated marks that the scan limit ended the walk before the boundary.
	truncated bool
}

func (i *boundedCommitIter) Next() (*object.Commit, error) {
	if i.done {
		return nil, io.EOF
	}

	c, err := i.src.Next()
	if err != nil {
		return nil, err
	}

	i.scanned++

	if _, atBoundary := i.boundary[c.Hash]; atBoundary {
		i.done = true
	} else if i.scanned >= maxScannedCommits {
		i.done = true
		i.truncated = true
	}

	return c, nil
}

func (i *boundedCommitIter) ForEach(cb func(*object.Commit) error) error {
	for {
		c, err := i.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}

		if err = cb(c); err != nil {
			if errors.Is(err, storer.ErrStop) {
				return nil
			}

			return err
		}
	}
}

func (i *boundedCommitIter) Close() { i.src.Close() }

// GetCommitsBetween returns commits reachable from newHash but not from oldHash,
// newest first, capped at maxCommits.
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

	boundary := commitBoundary(repo, oldHash, newHash)

	bounded := &boundedCommitIter{
		src:      object.NewCommitIterCTime(newCommit, nil, nil),
		boundary: boundary,
	}

	var iter object.CommitIter = bounded
	if pathFilter != nil {
		iter = object.NewCommitPathIterFromIter(pathFilter, bounded, false)
	}

	defer iter.Close()

	commits := make([]CommitInfo, 0, maxCommits)

	err = iter.ForEach(func(c *object.Commit) error {
		if _, atBoundary := boundary[c.Hash]; atBoundary || len(commits) >= maxCommits {
			return errStopWalk
		}

		commits = append(commits, newCommitInfo(c))

		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return nil, fmt.Errorf("failed to walk commit log: %w", err)
	}

	if bounded.truncated && len(commits) < maxCommits && log != nil {
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
