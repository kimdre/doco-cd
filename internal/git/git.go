package git

import (
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

var ErrRepositoryAlreadyExists = git.ErrRepositoryAlreadyExists

// CheckoutRepository checks out a specific commit in a given repository
func CheckoutRepository(path, ref, commitSHA string, skipTLSVerify bool) (*git.Repository, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return nil, err
	}

	// Fetch latest commits
	err = repo.Fetch(&git.FetchOptions{
		RefSpecs:        []git.ConfigRefSpec{git.ConfigRefSpec(ref)},
		Depth:           1,
		Force:           true,
		InsecureSkipTLS: skipTLSVerify,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, fmt.Errorf("failed to fetch repository: %w", err)
	}

	// Check if the commit exists
	_, err = repo.CommitObject(plumbing.NewHash(commitSHA))
	if err != nil {
		return nil, fmt.Errorf("commit not found: %w", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err
	}

	// Validate if the reference exists
	if ref != "" {
		err = worktree.Checkout(&git.CheckoutOptions{
			Branch: plumbing.ReferenceName(ref),
			Force:  true,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to checkout ref: %w", err)
		}
	}

	err = worktree.Checkout(&git.CheckoutOptions{
		Hash:  plumbing.NewHash(commitSHA),
		Force: true,
	})
	if err != nil {
		return nil, err
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
		URL:             url,
		SingleBranch:    true,
		ReferenceName:   plumbing.ReferenceName(ref),
		Tags:            git.NoTags,
		Depth:           1,
		InsecureSkipTLS: skipTLSVerify,
	})
}

// GetAuthUrl returns a clone URL with an access token for private repositories
func GetAuthUrl(url, authType, token string) string {
	// Retrieve the protocol from the clone URL (e.g. https://, http://, git://
	protocol := regexp.MustCompile("^(https?|git)://").FindString(url)
	return protocol + authType + ":" + token + "@" + url[len(protocol):]
}
