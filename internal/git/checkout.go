package git

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// CheckoutRepository checks out the specified reference in the repository, keeping untracked files intact.
// If cloneSubmodules is true, submodules will be initialized/updated using the provided auth.
// depth controls the shallow depth for submodule updates (0 = full).
func CheckoutRepository(repo *git.Repository, ref string, auth transport.AuthMethod, cloneSubmodules bool, depth ...int) error {
	worktree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	refSet, err := GetReferenceSet(repo, ref)
	if err != nil {
		return fmt.Errorf("failed to get reference set: %w", err)
	}

	if refSet.LocalRef == "" {
		return fmt.Errorf("%w: %s", ErrInvalidReference, ref)
	}

	// If RemoteRef is empty -> LocalRef is a commit SHA
	if refSet.RemoteRef == "" {
		hash := plumbing.NewHash(string(refSet.LocalRef))
		if err = worktree.Checkout(&git.CheckoutOptions{Hash: hash, Keep: true}); err != nil {
			return fmt.Errorf("failed to checkout commit: %w: %s", err, refSet.LocalRef)
		}
	} else {
		// Determine desired local branch reference (handle remote-only refs like refs/remotes/origin/<name>)
		desiredLocal := refSet.LocalRef
		if desiredLocal == refSet.RemoteRef && strings.HasPrefix(string(refSet.RemoteRef), "refs/remotes/"+RemoteName+"/") {
			branchName := strings.TrimPrefix(string(refSet.RemoteRef), "refs/remotes/"+RemoteName+"/")
			desiredLocal = plumbing.NewBranchReferenceName(branchName)
		}

		// Check existence of local ref
		_, localErr := repo.Reference(desiredLocal, true)
		if localErr != nil && !errors.Is(localErr, plumbing.ErrReferenceNotFound) {
			return fmt.Errorf("failed to resolve local reference %s: %w", desiredLocal, localErr)
		}

		// Use resolved remote hash; should be set by GetReferenceSet
		remoteHash := refSet.RemoteHash
		if remoteHash == plumbing.ZeroHash {
			// fallback attempt to resolve remote ref now
			if rRef, rErr := repo.Reference(refSet.RemoteRef, true); rErr == nil {
				remoteHash = rRef.Hash()
			}
		}

		// Branch behavior
		if strings.HasPrefix(string(desiredLocal), BranchPrefix) {
			if localErr == nil {
				// update existing local branch to point at remote hash (if available) so worktree ends up on fetched commit
				if remoteHash != plumbing.ZeroHash {
					newRef := plumbing.NewHashReference(desiredLocal, remoteHash)
					if err := repo.Storer.SetReference(newRef); err != nil {
						return fmt.Errorf("failed to update local branch %s to remote hash: %w", desiredLocal, err)
					}
				}

				if err = worktree.Checkout(&git.CheckoutOptions{Branch: desiredLocal, Keep: true}); err != nil {
					return fmt.Errorf("failed to checkout worktree: %w: %s", err, desiredLocal)
				}
			} else {
				// create local branch at remote hash and checkout it
				if err = worktree.Checkout(&git.CheckoutOptions{
					Branch: desiredLocal,
					Hash:   remoteHash,
					Create: true,
					Keep:   true,
				}); err != nil {
					return fmt.Errorf("failed to create and checkout branch %s: %w", desiredLocal, err)
				}
			}
		} else {
			// Fallback: detached checkout at remote hash (e.g. tags or remote-only refs that are not branches)
			commitHash, resolveErr := resolveCheckoutCommitHash(repo, remoteHash)
			if resolveErr != nil {
				return fmt.Errorf("failed to resolve commit for remote ref %s: %w", refSet.RemoteRef, resolveErr)
			}

			if err = worktree.Checkout(&git.CheckoutOptions{Hash: commitHash, Keep: true}); err != nil {
				return fmt.Errorf("failed to checkout commit for remote ref %s: %w", refSet.RemoteRef, err)
			}
		}
	}

	if err = ResetTrackedFiles(repo); err != nil {
		return fmt.Errorf("failed to reset tracked files: %w", err)
	}

	// Ensure submodules match the checked-out parent commit when requested.
	if cloneSubmodules {
		subDepth := 0
		if len(depth) > 0 {
			subDepth = depth[0]
		}

		if err = updateSubmodules(repo, auth, subDepth); err != nil {
			return fmt.Errorf("failed to update submodules: %w", err)
		}
	}

	return nil
}

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

// ResolveReferenceCommit resolves ref to the concrete commit hash it points
// to, without touching the repository's working tree or HEAD. It mirrors the
// resolution rules CheckoutRepository applies (branch/remote-tracking
// preference, annotated tag dereferencing, direct commit SHAs) so read-only
// callers land on exactly the commit a checkout of the same ref would.
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

		// No remote hash available (e.g. offline/local-only branch); fall back
		// to whatever the local branch currently points at, mirroring
		// CheckoutRepository's "update existing local branch" path when there
		// is nothing newer to move it to.
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
