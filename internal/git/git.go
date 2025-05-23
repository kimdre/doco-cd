package git

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/go-git/go-git/v5/config"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

const (
	RemoteName          = "origin"
	TagPrefix           = "refs/tags/"
	BranchPrefix        = "refs/heads/"
	MainBranch          = "refs/heads/main"
	refSpecAllBranches  = "+refs/heads/*:refs/remotes/origin/*"
	refSpecSingleBranch = "+refs/heads/%s:refs/remotes/origin/%s"
	refSpecAllTags      = "+refs/tags/*:refs/tags/*"
	refSpecSingleTag    = "+refs/tags/%s:refs/tags/%s"
)

var (
	ErrCheckoutFailed          = errors.New("failed to checkout repository")
	ErrFetchFailed             = errors.New("failed to fetch repository")
	ErrPullFailed              = errors.New("failed to pull repository")
	ErrRepositoryAlreadyExists = git.ErrRepositoryAlreadyExists
	ErrInvalidReference        = git.ErrInvalidReference
)

// UpdateRepository updates a local repository by
//  1. fetching the latest changes from the remote
//  2. checking out the specified reference (branch or tag)
//  3. pulling the latest changes from the remote
//  4. returning the updated repository
//
// Allowed reference forma
//   - Branches: refs/heads/main or main
//   - Tags: refs/tags/v1.0.0 or v1.0.0
func UpdateRepository(path, ref string, skipTLSVerify bool) (*git.Repository, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return nil, err
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err
	}

	// Fetch remote branches and tags
	err = repo.Fetch(&git.FetchOptions{
		RemoteName:      RemoteName,
		RefSpecs:        []config.RefSpec{refSpecAllBranches, refSpecAllTags},
		InsecureSkipTLS: skipTLSVerify,
		Prune:           true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, fmt.Errorf("%w: %w", ErrFetchFailed, err)
	}

	// Prepare the reference names for local and remote branches/tags
	type refCandidate struct {
		localRef  plumbing.ReferenceName
		remoteRef plumbing.ReferenceName
	}

	var refCandidates []refCandidate

	// Check if the reference is a branch or tag
	if strings.HasPrefix(ref, BranchPrefix) {
		name := strings.TrimPrefix(ref, BranchPrefix)

		refCandidates = append(refCandidates, refCandidate{
			localRef:  plumbing.NewBranchReferenceName(name),
			remoteRef: plumbing.NewRemoteReferenceName(RemoteName, name),
		})
	} else if strings.HasPrefix(ref, TagPrefix) {
		name := strings.TrimPrefix(ref, TagPrefix)

		refCandidates = append(refCandidates, refCandidate{
			localRef:  plumbing.NewTagReferenceName(name),
			remoteRef: plumbing.NewTagReferenceName(name),
		})
	} else {
		// Create ref candidate for branch and tag
		refCandidates = append(refCandidates,
			refCandidate{
				// Create ref candidate for branch
				localRef:  plumbing.NewBranchReferenceName(ref),
				remoteRef: plumbing.NewRemoteReferenceName(RemoteName, ref),
			},
			// Create ref candidate for tag
			refCandidate{
				localRef:  plumbing.NewTagReferenceName(ref),
				remoteRef: plumbing.NewTagReferenceName(ref),
			},
		)
	}

	var successCandidate refCandidate

	for _, candidate := range refCandidates {
		if candidate.localRef.IsBranch() {
			newRef := plumbing.NewSymbolicReference(candidate.localRef, candidate.remoteRef)

			err = repo.Storer.SetReference(newRef)
			if err != nil {
				return nil, err
			}
		}
		// Check if localRef exists remotely
		_, err = repo.Reference(candidate.remoteRef, true)
		if err != nil {
			if errors.Is(err, plumbing.ErrReferenceNotFound) {
				// If the reference does not exist, continue to the next candidate
				continue
			}

			return nil, fmt.Errorf("%w: %s", err, candidate.localRef)
		}

		successCandidate = candidate
	}

	if successCandidate.localRef == "" {
		return nil, fmt.Errorf("%w: %s", ErrInvalidReference, ref)
	}

	// Check if the reference is already checked out
	checkedOutRef, err := repo.Head()
	if err != nil {
		return nil, err
	}

	if checkedOutRef.Name() == successCandidate.localRef || checkedOutRef.Name() == successCandidate.remoteRef {
		// If the reference is already checked out, do a pull and return
		fmt.Println("Already checked out", successCandidate.localRef, ", pulling latest changes")

		err = worktree.Pull(&git.PullOptions{
			RemoteName:      RemoteName,
			InsecureSkipTLS: skipTLSVerify,
			SingleBranch:    true,
		})
		if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return nil, fmt.Errorf("%w: %w", ErrPullFailed, err)
		}

		return repo, nil
	}

	fmt.Println("Checking out new reference", successCandidate.localRef)

	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: successCandidate.localRef,
		Keep:   true,
		// Force:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w: %s", ErrCheckoutFailed, err, successCandidate.localRef)
	}

	return repo, nil
}

// CloneRepository clones a repository from a given URL and reference to a temporary directory
func CloneRepository(path, url, ref string, skipTLSVerify bool) (*git.Repository, error) {
	err := os.MkdirAll(path, os.ModePerm)
	if err != nil {
		return nil, err
	}

	return git.PlainClone(path, false, &git.CloneOptions{
		RemoteName:      RemoteName,
		URL:             url,
		SingleBranch:    true,
		ReferenceName:   plumbing.ReferenceName(ref),
		Tags:            git.NoTags,
		InsecureSkipTLS: skipTLSVerify,
	})
}

// GetAuthUrl returns a clone URL with an access token for private repositories
func GetAuthUrl(url, authType, token string) string {
	// Retrieve the protocol from the clone URL (e.g. https://, http://, git://
	protocol := regexp.MustCompile("^(https?|git)://").FindString(url)
	return protocol + authType + ":" + token + "@" + url[len(protocol):]
}
