package git

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// resolveCheckoutCommitHash dereferences annotated tags so a detached checkout
// records the target commit, matching Git's checkout behavior.
func resolveCheckoutCommitHash(repo *git.Repository, hash plumbing.Hash) (plumbing.Hash, error) {
	for {
		obj, err := repo.Object(plumbing.AnyObject, hash)
		if err != nil {
			return plumbing.ZeroHash, err
		}

		tag, ok := obj.(*object.Tag)
		if !ok {
			return hash, nil
		}

		hash = tag.Target
	}
}

// ResolveReferenceCommit resolves ref to the commit hash it points to,
// without touching the working tree or HEAD.
func ResolveReferenceCommit(repo *git.Repository, ref string) (plumbing.Hash, error) {
	refSet, err := GetReferenceSet(repo, ref)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get reference set: %w", err)
	}

	if refSet.LocalRef == "" {
		return plumbing.ZeroHash, fmt.Errorf("%w: %s", ErrInvalidReference, ref)
	}

	// If RemoteRef is empty -> LocalRef is a commit SHA.
	if refSet.RemoteRef == "" {
		return resolveCheckoutCommitHash(repo, plumbing.NewHash(string(refSet.LocalRef)))
	}

	desiredLocal := refSet.LocalRef
	if desiredLocal == refSet.RemoteRef && strings.HasPrefix(string(refSet.RemoteRef), "refs/remotes/"+RemoteName+"/") {
		branchName := strings.TrimPrefix(string(refSet.RemoteRef), "refs/remotes/"+RemoteName+"/")
		desiredLocal = plumbing.NewBranchReferenceName(branchName)
	}

	remoteHash := refSet.RemoteHash
	if remoteHash == plumbing.ZeroHash {
		if rRef, rErr := repo.Reference(refSet.RemoteRef, true); rErr == nil {
			remoteHash = rRef.Hash()
		}
	}

	if strings.HasPrefix(string(desiredLocal), BranchPrefix) {
		if remoteHash != plumbing.ZeroHash {
			return resolveCheckoutCommitHash(repo, remoteHash)
		}

		// No remote hash (e.g. offline/local-only branch); fall back to the
		// local branch's current commit, since there's nothing newer to
		// resolve it to.
		localRef, localErr := repo.Reference(desiredLocal, true)
		if localErr != nil {
			return plumbing.ZeroHash, fmt.Errorf("failed to resolve local reference %s: %w", desiredLocal, localErr)
		}

		return resolveCheckoutCommitHash(repo, localRef.Hash())
	}

	// Fallback: tags or remote-only refs that are not branches.
	commitHash, resolveErr := resolveCheckoutCommitHash(repo, remoteHash)
	if resolveErr != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to resolve commit for remote ref %s: %w", refSet.RemoteRef, resolveErr)
	}

	return commitHash, nil
}
