package git

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
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
			if err = worktree.Checkout(&git.CheckoutOptions{Hash: remoteHash, Keep: true}); err != nil {
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
