package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/diff"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

const (
	RemoteName          = "origin"
	TagPrefix           = "refs/tags/"
	BranchPrefix        = "refs/heads/"
	MainBranch          = "refs/heads/main"
	SwarmModeBranch     = "refs/heads/swarm-mode"
	refSpecAllBranches  = "+refs/heads/*:refs/remotes/origin/*"
	refSpecSingleBranch = "+refs/heads/%s:refs/remotes/origin/%s"
	refSpecAllTags      = "+refs/tags/*:refs/tags/*"
	refSpecSingleTag    = "+refs/tags/%s:refs/tags/%s"
)

var (
	ErrCheckoutFailed          = errors.New("failed To checkout repository")
	ErrFetchFailed             = errors.New("failed To fetch repository")
	ErrPullFailed              = errors.New("failed To pull repository")
	ErrRepositoryAlreadyExists = git.ErrRepositoryAlreadyExists
	ErrInvalidReference        = git.ErrInvalidReference
)

// ChangedFile represents a file that has changed between two commits.
type ChangedFile struct {
	// From represents the file state before the change.
	From diff.File
	// To represents the file state after the change.
	To diff.File
}

type RefSet struct {
	localRef  plumbing.ReferenceName
	remoteRef plumbing.ReferenceName
}

// GetReferenceSet retrieves a RefSet of local and remote references for a given reference name.
func GetReferenceSet(repo *git.Repository, ref string) (RefSet, error) {
	var refCandidates []RefSet

	// Check if the reference is a branch or tag
	switch {
	case strings.HasPrefix(ref, BranchPrefix):
		name := strings.TrimPrefix(ref, BranchPrefix)

		refCandidates = append(refCandidates, RefSet{
			localRef:  plumbing.NewBranchReferenceName(name),
			remoteRef: plumbing.NewRemoteReferenceName(RemoteName, name),
		})
	case strings.HasPrefix(ref, TagPrefix):
		name := strings.TrimPrefix(ref, TagPrefix)

		refCandidates = append(refCandidates, RefSet{
			localRef:  plumbing.NewTagReferenceName(name),
			remoteRef: plumbing.NewTagReferenceName(name),
		})
	default:
		// Create ref candidate for branch and tag
		refCandidates = append(refCandidates,
			RefSet{
				// Create ref candidate for branch
				localRef:  plumbing.NewBranchReferenceName(ref),
				remoteRef: plumbing.NewRemoteReferenceName(RemoteName, ref),
			},
			// Create ref candidate for tag
			RefSet{
				localRef:  plumbing.NewTagReferenceName(ref),
				remoteRef: plumbing.NewTagReferenceName(ref),
			},
		)
	}

	for _, candidate := range refCandidates {
		if candidate.localRef.IsBranch() {
			newRef := plumbing.NewSymbolicReference(candidate.localRef, candidate.remoteRef)

			err := repo.Storer.SetReference(newRef)
			if err != nil {
				return RefSet{}, err
			}
		}
		// Check if localRef exists remotely
		_, err := repo.Reference(candidate.remoteRef, true)
		if err != nil {
			if errors.Is(err, plumbing.ErrReferenceNotFound) {
				// If the reference does not exist, continue To the next candidate
				continue
			}

			return RefSet{}, fmt.Errorf("%w: %s", err, candidate.localRef)
		}

		return candidate, nil
	}

	return RefSet{}, fmt.Errorf("%w: %s", ErrInvalidReference, ref)
}

// UpdateRepository updates a local repository by
//  1. fetching the latest changes From the remote
//  2. checking out the specified reference (branch or tag)
//  3. pulling the latest changes From the remote
//  4. returning the updated repository
//
// Allowed reference forma
//   - Branches: refs/heads/main or main
//   - Tags: refs/tags/v1.0.0 or v1.0.0
func UpdateRepository(path, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions) (*git.Repository, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return nil, err
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err
	}

	opts := &git.FetchOptions{
		RemoteName:      RemoteName,
		RemoteURL:       url,
		RefSpecs:        []config.RefSpec{refSpecAllBranches, refSpecAllTags},
		InsecureSkipTLS: skipTLSVerify,
		Prune:           true,
	}

	if proxyOpts != (transport.ProxyOptions{}) {
		opts.ProxyOptions = proxyOpts
	}

	// Fetch remote branches and tags
	err = repo.Fetch(opts)
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, fmt.Errorf("%w: %w", ErrFetchFailed, err)
	}

	refSet, err := GetReferenceSet(repo, ref)
	if err != nil {
		return nil, err
	}

	if refSet.localRef == "" {
		return nil, fmt.Errorf("%w: %s", ErrInvalidReference, ref)
	}

	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: refSet.localRef,
		Keep:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w: %s", ErrCheckoutFailed, err, refSet.localRef)
	}

	err = ResetTrackedFiles(worktree)
	if err != nil {
		return nil, fmt.Errorf("failed To reset tracked files: %w", err)
	}

	return repo, nil
}

// CloneRepository clones a repository From a given URL and reference To a temporary directory.
func CloneRepository(path, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions) (*git.Repository, error) {
	err := os.MkdirAll(path, filesystem.PermDir)
	if err != nil {
		return nil, err
	}

	opts := &git.CloneOptions{
		RemoteName:      RemoteName,
		URL:             url,
		SingleBranch:    true,
		ReferenceName:   plumbing.ReferenceName(ref),
		Tags:            git.NoTags,
		InsecureSkipTLS: skipTLSVerify,
	}

	if proxyOpts != (transport.ProxyOptions{}) {
		opts.ProxyOptions = proxyOpts
	}

	return git.PlainClone(path, false, opts)
}

// GetAuthUrl returns a clone URL with an access token for private repositories.
func GetAuthUrl(url, authType, token string) string {
	// Retrieve the protocol From the clone URL (e.g. https://, http://, git://
	protocol := regexp.MustCompile("^(https?|git)://").FindString(url)

	return protocol + authType + ":" + token + "@" + url[len(protocol):]
}

// GetLatestCommit retrieves the last commit hash for a given reference in a repository.
func GetLatestCommit(repo *git.Repository, ref string) (string, error) {
	// Get the reference for the specified ref
	refSet, err := GetReferenceSet(repo, ref)
	if err != nil {
		return plumbing.ZeroHash.String(), err
	}

	r, err := repo.Reference(refSet.remoteRef, true)
	if err != nil {
		return plumbing.ZeroHash.String(), fmt.Errorf("failed To get reference %s: %w", ref, err)
	}

	// Get the commit object for the reference
	commit, err := repo.CommitObject(r.Hash())
	if err != nil {
		return plumbing.ZeroHash.String(), fmt.Errorf("failed To get commit object for %s: %w", r.Hash(), err)
	}

	return commit.Hash.String(), nil
}

// ResetTrackedFiles resets all tracked files in the worktree To their last committed state
// while leaving untracked files intact.
func ResetTrackedFiles(worktree *git.Worktree) error {
	changedFiles, err := worktree.Status()
	if err != nil {
		return fmt.Errorf("failed To get worktree status: %w", err)
	}

	resetFiles := make([]string, 0, len(changedFiles))

	for file, status := range changedFiles {
		if status.Staging != git.Untracked {
			resetFiles = append(resetFiles, file)
		}
	}

	if len(resetFiles) > 0 {
		err = worktree.Reset(&git.ResetOptions{
			Mode:  git.HardReset,
			Files: resetFiles,
		})
		if err != nil {
			return fmt.Errorf("failed To reset worktree: %w", err)
		}
	}

	return nil
}

// GetChangedFilesBetweenCommits retrieves a list of changed files between two commits in a repository.
func GetChangedFilesBetweenCommits(repo *git.Repository, commitHash1, commitHash2 plumbing.Hash) ([]ChangedFile, error) {
	commit1, err := repo.CommitObject(commitHash1)
	if err != nil {
		return nil, fmt.Errorf("failed To get commit From commitHash1 %s: %w", commitHash1, err)
	}

	commit2, err := repo.CommitObject(commitHash2)
	if err != nil {
		return nil, fmt.Errorf("failed To get commit From commitHash2 %s: %w", commitHash2, err)
	}

	// Create a patch between the two commits
	patch, err := commit1.Patch(commit2)
	if err != nil {
		return nil, fmt.Errorf("failed To create patch: %w", err)
	}

	changedFiles := make([]ChangedFile, 0, len(patch.FilePatches()))
	for _, file := range patch.FilePatches() {
		from, to := file.Files()
		changedFiles = append(changedFiles, ChangedFile{From: from, To: to})
	}

	return changedFiles, nil
}

// HasChangesInSubdir checks if any of the changed files are in a specified subdirectory.
func HasChangesInSubdir(changedFiles []ChangedFile, subdir string) (bool, error) {
	for _, file := range changedFiles {
		var paths []string

		if file.From != nil {
			paths = append(paths, file.From.Path())
		}

		if file.To != nil {
			paths = append(paths, file.To.Path())
		}

		for _, p := range paths {
			rel, err := filepath.Rel(subdir, p)
			if err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
				return true, nil
			}
		}
	}

	return false, nil
}
