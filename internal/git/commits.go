package git

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
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

// GetCommitsBetween returns commits reachable from newHash but not from oldHash,
// newest first, capped at maxCommits.
func GetCommitsBetween(repo *git.Repository, oldHash, newHash plumbing.Hash, maxCommits int) ([]CommitInfo, error) {
	iter, err := repo.Log(&git.LogOptions{From: newHash, Order: git.LogOrderCommitterTime})
	if err != nil {
		return nil, fmt.Errorf("failed to read commit log from %s: %w", newHash, err)
	}
	defer iter.Close()

	boundary := commitBoundary(repo, oldHash, newHash)
	commits := make([]CommitInfo, 0, maxCommits)

	stop := errors.New("stop")

	err = iter.ForEach(func(c *object.Commit) error {
		if _, atBoundary := boundary[c.Hash]; atBoundary || len(commits) >= maxCommits {
			return stop
		}

		subject := c.Message
		if i := strings.IndexByte(subject, '\n'); i >= 0 {
			subject = subject[:i]
		}

		commits = append(commits, CommitInfo{
			Hash:      c.Hash.String(),
			ShortHash: c.Hash.String()[:DefaultShortSHALength],
			Subject:   strings.TrimSpace(subject),
			Author:    c.Author.Name,
		})

		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return nil, fmt.Errorf("failed to walk commit log: %w", err)
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
