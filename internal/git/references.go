package git

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// GetReferenceSet resolves ref to the local and remote plumbing.ReferenceName
// to use during checkout, along with the resolved remote commit hash.
//
// Resolution order (first match wins):
//  1. Commit SHA: returned directly; no reference store lookup is performed.
//  2. For refs/-prefixed names: the exact name, then its remote-tracking counterpart.
//  3. For short names: refs/heads/<ref>, refs/remotes/origin/<ref>, refs/tags/<ref>,
//     then the bare name (which only resolves for uppercase pseudo-refs like HEAD).
//
// Candidates whose name cannot be stored safely (see plumbing.ReferenceName.IsSafe)
// are skipped rather than queried, so a malformed ref yields ErrInvalidReference
// instead of a storage-layer error. Any other storage error is treated as a
// transient failure and returned immediately.
func GetReferenceSet(repo *git.Repository, ref string) (RefSet, error) {
	// Commit SHAs are used directly because there is no reference name to resolve.
	if plumbing.IsHash(ref) {
		return RefSet{LocalRef: plumbing.ReferenceName(ref)}, nil
	}

	type candidate struct {
		local  plumbing.ReferenceName
		remote plumbing.ReferenceName
	}

	var candidates []candidate

	if strings.HasPrefix(ref, "refs/") {
		fullRef := plumbing.ReferenceName(ref)

		remoteRef := fullRef
		if after, ok := strings.CutPrefix(ref, BranchPrefix); ok {
			remoteRef = plumbing.NewRemoteReferenceName(RemoteName, after)
		}

		candidates = append(candidates,
			candidate{fullRef, remoteRef},
			candidate{remoteRef, remoteRef},
		)
	} else {
		remoteRef := plumbing.NewRemoteReferenceName(RemoteName, ref)
		candidates = append(candidates,
			candidate{plumbing.NewBranchReferenceName(ref), remoteRef},
			candidate{remoteRef, remoteRef},
			candidate{plumbing.NewTagReferenceName(ref), plumbing.NewTagReferenceName(ref)},
			// The bare name only ever resolves for uppercase pseudo-refs such as
			// HEAD or ORIG_HEAD; the IsSafe filter below discards it otherwise.
			candidate{plumbing.ReferenceName(ref), plumbing.ReferenceName(ref)},
		)
	}

	for _, c := range candidates {
		// go-git v5.19.2+ validates reference names at the storage layer and rejects
		// unsafe ones (not under refs/, escaping it, or not an uppercase pseudo-ref)
		// with an error distinct from plumbing.ErrReferenceNotFound. Such a name can
		// never exist, so skip the lookup instead of misreporting it as transient.
		if !c.local.IsSafe() {
			continue
		}

		localRef, err := repo.Reference(c.local, true)
		if err == nil {
			remoteHash := plumbing.ZeroHash

			switch {
			case c.remote == c.local:
				// Already resolved above; avoid a redundant store lookup.
				remoteHash = localRef.Hash()
			case c.remote.IsSafe():
				rRef, rErr := repo.Reference(c.remote, true)
				if rErr == nil {
					remoteHash = rRef.Hash()
				} else if !errors.Is(rErr, plumbing.ErrReferenceNotFound) {
					return RefSet{}, fmt.Errorf("failed to get reference %s: %w", c.remote, rErr)
				} else if c.local.IsBranch() {
					// Branches supplied to deployment synchronization must still
					// exist on origin; otherwise a same-named tag must win.
					continue
				}
			}

			return RefSet{LocalRef: c.local, RemoteRef: c.remote, RemoteHash: remoteHash}, nil
		}

		if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return RefSet{}, fmt.Errorf("failed to get reference %s: %w", ref, err)
		}
	}

	return RefSet{}, fmt.Errorf("%w: %s", ErrInvalidReference, ref)
}

func fetchedReferenceExistsAfterFetch(repo *git.Repository, ref string) (bool, error) {
	if plumbing.IsHash(ref) {
		return true, nil
	}

	refName := plumbing.ReferenceName(ref)
	if !strings.HasPrefix(ref, "refs/") && refName.IsSafe() {
		// Uppercase pseudo-refs such as HEAD are resolved locally rather than fetched.
		return true, nil
	}

	candidates := make([]plumbing.ReferenceName, 0, 3)

	switch {
	case strings.HasPrefix(ref, BranchPrefix):
		branch := strings.TrimPrefix(ref, BranchPrefix)
		candidates = append(candidates, plumbing.NewRemoteReferenceName(RemoteName, branch))
	case strings.HasPrefix(ref, TagPrefix):
		candidates = append(candidates, plumbing.ReferenceName(ref))
	case strings.HasPrefix(ref, "refs/"):
		candidates = append(candidates, plumbing.ReferenceName(ref))
	default:
		candidates = append(candidates,
			plumbing.NewRemoteReferenceName(RemoteName, ref),
			plumbing.NewTagReferenceName(ref),
		)
	}

	for _, candidate := range candidates {
		if _, err := repo.Reference(candidate, true); err == nil {
			return true, nil
		} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return false, err
		}
	}

	return false, nil
}

// MatchesHead inspects an existing repository at path and determines if HEAD is at the specified reference (branch, tag, or commit SHA).
func MatchesHead(path, ref string) (bool, error) {
	repo, err := OpenRepository(path)
	if err != nil {
		if errors.Is(err, git.ErrRepositoryNotExists) {
			return false, nil
		}

		return false, fmt.Errorf("failed to open repository at %s: %w", path, err)
	}

	return repositoryMatchesHead(repo, ref)
}

// repositoryMatchesHead checks if the given repository's HEAD matches the specified reference (branch, tag, or commit SHA).
func repositoryMatchesHead(repo *git.Repository, ref string) (bool, error) {
	head, err := repo.Head()
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrGetHeadFailed, err)
	}

	refSet, err := GetReferenceSet(repo, ref)
	if err != nil {
		return false, fmt.Errorf("failed to get reference set for %s: %w", ref, err)
	}

	// If RemoteRef is empty, LocalRef is a commit SHA
	if refSet.RemoteRef == "" {
		return head.Hash().String() == string(refSet.LocalRef), nil
	}

	r, err := repo.Reference(refSet.RemoteRef, true)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return false, nil
		}

		return false, fmt.Errorf("failed to get reference %s: %w", refSet.RemoteRef, err)
	}

	return head.Hash() == r.Hash(), nil
}

// HeadMatchesCommit reports whether the repository at path has exactly the given
// commit SHA at HEAD. Returns false (no error) when the repository does not exist
// or HEAD cannot be resolved, so callers can treat a false result as "go ahead and fetch".
func HeadMatchesCommit(repoPath, commitSHA string) (bool, error) {
	commitSHA = strings.TrimSpace(commitSHA)
	if commitSHA == "" {
		return false, nil
	}

	repo, err := OpenRepository(repoPath)
	if err != nil {
		if errors.Is(err, git.ErrRepositoryNotExists) {
			return false, nil
		}

		return false, fmt.Errorf("failed to open repository at %s: %w", repoPath, err)
	}

	head, err := repo.Head()
	if err != nil {
		return false, fmt.Errorf("%w for repository '%s': %w", ErrGetHeadFailed, repoPath, err)
	}

	return head.Hash().String() == commitSHA, nil
}
