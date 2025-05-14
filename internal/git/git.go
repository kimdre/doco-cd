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

const RemoteName = "origin"

var (
	ErrRepositoryAlreadyExists = git.ErrRepositoryAlreadyExists
	ErrCommitNotFound          = errors.New("commit not found")
	ErrCheckoutCommitFailed    = errors.New("failed to checkout commit")
	ErrCheckoutRefFailed       = errors.New("failed to checkout ref")
)

// CheckoutRepository checks out a specific commit in a given repository
func CheckoutRepository(path, ref, commitSHA string, skipTLSVerify bool) (*git.Repository, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return nil, err
	}

	err = repo.Fetch(&git.FetchOptions{
		RemoteName:      RemoteName,
		RefSpecs:        []config.RefSpec{"+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*"},
		Force:           true,
		Tags:            git.AllTags,
		InsecureSkipTLS: skipTLSVerify,
		Progress:        nil,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, fmt.Errorf("failed to fetch repository: %w", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err
	}

	if commitSHA == "" {
		if ref == "" {
			return nil, errors.New("ref is not set")
		}

		var refCandidates []plumbing.ReferenceName
		if strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/") {
			refCandidates = append(refCandidates, plumbing.ReferenceName(ref))
		} else {
			refCandidates = append(refCandidates,
				plumbing.ReferenceName("refs/heads/"+ref),
				plumbing.ReferenceName("refs/tags/"+ref))
		}

		var checkoutErr error
		for _, refName := range refCandidates {
			checkoutErr = worktree.Checkout(&git.CheckoutOptions{
				Branch: refName,
				Force:  true,
			})
			if checkoutErr == nil {
				break
			}
		}

		if checkoutErr != nil {
			return nil, fmt.Errorf("%w: %s", ErrCheckoutRefFailed, checkoutErr)
		}
	} else {
		hash := plumbing.NewHash(commitSHA)

		_, err = repo.CommitObject(hash)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCommitNotFound, err)
		}

		err = worktree.Checkout(&git.CheckoutOptions{
			Hash:  hash,
			Force: true,
		})
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCheckoutCommitFailed, err)
		}
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
		Progress:        nil,
	})
}

// GetAuthUrl returns a clone URL with an access token for private repositories
func GetAuthUrl(url, authType, token string) string {
	// Retrieve the protocol from the clone URL (e.g. https://, http://, git://
	protocol := regexp.MustCompile("^(https?|git)://").FindString(url)
	return protocol + authType + ":" + token + "@" + url[len(protocol):]
}
