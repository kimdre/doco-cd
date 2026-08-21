package git

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/common/errdecode"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git/ssh"
)

const (
	DefaultShortSHALength = 7 // Default length for shortened commit SHAs
	RemoteName            = "origin"
	TagPrefix             = "refs/tags/"
	BranchPrefix          = "refs/heads/"
	MainBranch            = "refs/heads/main"
	SwarmModeBranch       = "refs/heads/swarm-mode"
	refSpecAllBranches    = "+refs/heads/*:refs/remotes/origin/*"
	refSpecSingleBranch   = "+refs/heads/%s:refs/remotes/origin/%s"
	refSpecAllTags        = "+refs/tags/*:refs/tags/*"
	refSpecSingleTag      = "+refs/tags/%s:refs/tags/%s"
)

var (
	ErrMissingAuthToken           = errors.New("missing access token")
	ErrCheckoutFailed             = errors.New("failed to checkout repository")
	ErrFetchFailed                = errors.New("failed to fetch repository")
	ErrRepositoryNotExists        = git.ErrRepositoryNotExists
	ErrRepositoryAlreadyExists    = git.ErrRepositoryAlreadyExists
	ErrInvalidReference           = git.ErrInvalidReference
	ErrSSHKeyRequired             = errors.New("ssh URL requires SSH_PRIVATE_KEY to be set")
	ErrPossibleAuthMethodMismatch = errors.New("there might be a mismatch between the authentication method and the repository or submodule remote URL")
	ErrRemoteURLMismatch          = errors.New("remote URL does not match expected URL")
	ErrGetHeadFailed              = errors.New("failed to get HEAD reference")
)

// retrier is a shared retry configuration for git operations that may fail
// due to transient issues like network errors or temporary repository states.
var retrier = retry.New(
	retry.Attempts(3),
	retry.Delay(250*time.Millisecond),
	retry.DelayType(retry.BackOffDelay),
	retry.RetryIf(func(err error) bool {
		_, isURLErr := errors.AsType[*url.Error](err)
		netErr, isNetErr := errors.AsType[net.Error](err)

		return isURLErr || (isNetErr && netErr.Timeout())
	}),
)

// ChangedFile represents a file that has changed between two commits.
type ChangedFile struct {
	// From represents the file state before the change.
	From diff.File
	// To represents the file state after the change.
	To diff.File
}

type RefSet struct {
	LocalRef   plumbing.ReferenceName
	RemoteRef  plumbing.ReferenceName
	RemoteHash plumbing.Hash
}

// GetReferenceSet resolves ref to the local and remote plumbing.ReferenceName
// to use during checkout, along with the resolved remote commit hash.
//
// Resolution order (first match wins):
//  1. Commit SHA — returned directly; no reference store lookup is performed.
//  2. For refs/-prefixed names: the exact name, then its remote-tracking counterpart.
//  3. For short names: refs/heads/<ref>, refs/remotes/origin/<ref>, refs/tags/<ref>,
//     then the bare name (which only resolves for uppercase pseudo-refs like HEAD).
//
// Candidates whose name cannot be stored safely (see plumbing.ReferenceName.IsSafe)
// are skipped rather than queried, so a malformed ref yields ErrInvalidReference
// instead of a storage-layer error. Any other storage error is treated as a
// transient failure and returned immediately.
func GetReferenceSet(repo *git.Repository, ref string) (RefSet, error) {
	// Commit SHAs are used directly — there is no reference name to resolve.
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
				if rRef, rErr := repo.Reference(c.remote, true); rErr == nil {
					remoteHash = rRef.Hash()
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

// ConvertSSHUrl converts SSH URLs to the ssh:// format.
// e.g. convert git@github.com:user/repo.git to ssh://git@github.com/user/repo.git
func ConvertSSHUrl(url string) string {
	// Check if url starts with git@ and convert to ssh:// format
	if strings.HasPrefix(url, "git@") {
		// Replace the first ':' with '/' after the host
		if idx := strings.Index(url, ":"); idx != -1 {
			url = url[:idx] + "/" + url[idx+1:]
		}

		url = "ssh://" + url
	}

	return url
}

// updateRemoteURL updates the remote URL of the repository.
func updateRemoteURL(repo *git.Repository, url string) error {
	// Update remote URL in case it has changed
	remote, err := repo.Remote(RemoteName)
	if err != nil {
		// If remote does not exist, create it with the provided URL
		c := &config.RemoteConfig{Name: RemoteName}
		if IsSSH(url) {
			c.URLs = []string{ConvertSSHUrl(url)}
		} else {
			c.URLs = []string{url}
		}

		_, createErr := repo.CreateRemote(c)
		if createErr != nil {
			return fmt.Errorf("failed to create remote %s: %w", RemoteName, createErr)
		}

		return nil
	}

	c := remote.Config()

	var newUrl []string
	if IsSSH(url) {
		newUrl = []string{ConvertSSHUrl(url)}
	} else {
		newUrl = []string{url}
	}

	if slices.Compare(c.URLs, newUrl) == 0 {
		// No change in URL
		return nil
	}

	c.URLs = newUrl

	err = repo.DeleteRemote(RemoteName)
	if err != nil {
		return fmt.Errorf("failed to delete remote %s: %w", RemoteName, err)
	}

	_, err = repo.CreateRemote(c)
	if err != nil {
		return fmt.Errorf("failed to create remote %s: %w", RemoteName, err)
	}

	return nil
}

// OpenRepository opens an existing git repository at the specified path.
// This is a lightweight operation that doesn't fetch or update the repository.
func OpenRepository(path string) (*git.Repository, error) {
	return git.PlainOpen(path)
}

// FetchRepository fetches updates from the remote repository, including all branches and tags, and prunes deleted references.
// If depth > 0, a shallow fetch is performed with the specified number of commits.
func FetchRepository(repo *git.Repository, url string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, depth int) error {
	opts := &git.FetchOptions{
		RemoteName: RemoteName,
		RemoteURL:  url,
		RefSpecs:   []config.RefSpec{refSpecAllBranches, refSpecAllTags},
		Prune:      true,
		Auth:       auth,
		Depth:      depth,
	}

	// SSH auth when key is provided
	if IsSSH(url) {
		err := ssh.AddToKnownHosts(url)
		if err != nil {
			return fmt.Errorf("failed to add host to known_hosts: %w", err)
		}

		opts.RemoteURL = ConvertSSHUrl(url)
	} else if !IsLocalFile(url) {
		opts.InsecureSkipTLS = skipTLSVerify

		if proxyOpts != (transport.ProxyOptions{}) {
			opts.ProxyOptions = proxyOpts
		}
	}

	fetchWithRetry := func() error {
		return retrier.Do(
			func() error {
				err := repo.Fetch(opts)
				if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
					return err
				}

				return nil
			})
	}

	err := fetchWithRetry()
	if err != nil && IsSSH(url) && ssh.IsHostKeyMismatchError(err) {
		if refreshErr := ssh.RefreshKnownHost(url); refreshErr != nil {
			return fmt.Errorf("failed to refresh host key after mismatch: %w", refreshErr)
		}

		err = fetchWithRetry()
	}

	return err
}

// UpdateRepository fetches and checks out the requested ref.
// If depth > 0, shallow fetches are used. When the requested ref is not reachable
// within the current depth, the repository is incrementally deepened before falling
// back to a full fetch.
func UpdateRepository(path, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, cloneSubmodules bool, depth int) (*git.Repository, error) {
	// Serialize operations on the same path
	unlock := AcquirePathLock(path)
	defer unlock()

	repo, err := git.PlainOpen(path)
	if err != nil {
		return nil, err
	}

	// Detect shallow/full transition and re-clone if needed
	if needsReclone(path, depth) {
		slog.Info("git depth configuration changed, re-cloning repository",
			slog.String("path", path),
			slog.Int("requested_depth", depth))

		unlock() // release lock before re-clone (CloneRepository acquires its own)

		if err := os.RemoveAll(path); err != nil {
			return nil, fmt.Errorf("failed to remove repository for re-clone: %w", err)
		}

		return CloneRepository(path, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
	}

	err = updateRemoteURL(repo, url)
	if err != nil {
		return nil, err
	}

	err = FetchRepository(repo, url, skipTLSVerify, proxyOpts, auth, depth)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFetchFailed, err)
	}

	// Pass auth and cloneSubmodules so CheckoutRepository can ensure submodules are updated when needed.
	err = CheckoutRepository(repo, ref, auth, cloneSubmodules)
	if err != nil {
		// Check if this looks like repository corruption (ref not found despite successful fetch)
		if IsCorruptionError(err) {
			fetchedExists, fetchedCheckErr := fetchedReferenceExistsAfterFetch(repo, ref)
			if fetchedCheckErr != nil {
				slog.Warn("failed to validate requested reference in fetched refs before repair",
					slog.String("path", path),
					slog.String("ref", ref),
					slog.String("error", FormatGitErrorMessage(fetchedCheckErr)))
			}

			if fetchedCheckErr == nil && !fetchedExists {
				slog.Warn("requested reference does not exist in fetched refs, skipping local repair",
					slog.String("path", path),
					slog.String("ref", ref))

				return nil, fmt.Errorf("%w: %w", ErrCheckoutFailed, err)
			}

			slog.Warn("detected possible repository corruption during checkout",
				slog.String("path", path),
				slog.String("error", FormatGitErrorMessage(err)))

			// Release the lock before calling RepairRepository because repair may re-clone.
			unlock()

			repairedRepo, repairErr := RepairRepository(path, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth, slog.Default())
			if repairErr == nil {
				return repairedRepo, nil
			}

			slog.Error("failed to repair corrupted repository",
				slog.String("path", path),
				slog.String("repair_error", FormatGitErrorMessage(repairErr)))
		}
		// Attempt to deepen if the ref is unreachable in a shallow clone
		if depth > 0 && IsRefUnreachableError(err) {
			repo, deepenErr := deepenAndCheckout(repo, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
			if deepenErr != nil {
				return nil, fmt.Errorf("%w: %w", ErrCheckoutFailed, deepenErr)
			}

			return repo, nil
		}

		return nil, fmt.Errorf("%w: %w", ErrCheckoutFailed, err)
	}

	return repo, nil
}

func fetchedReferenceExistsAfterFetch(repo *git.Repository, ref string) (bool, error) {
	if plumbing.IsHash(ref) {
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

// CloneRepository clones a repository with HTTP or SSH auth.
// If depth > 0, a shallow clone is performed with the specified number of commits.
func CloneRepository(path, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, cloneSubmodules bool, depth int) (*git.Repository, error) {
	// Serialize operations on the same path to avoid concurrent partial clones
	unlock := AcquirePathLock(path)
	defer unlock()

	err := os.MkdirAll(path, filesystem.PermDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", path, err)
	}

	opts := &git.CloneOptions{
		RemoteName: RemoteName,
		URL:        url,
		Tags:       git.AllTags,
		Auth:       auth,
		Depth:      depth,
	}

	if cloneSubmodules {
		opts.RecurseSubmodules = git.DefaultSubmoduleRecursionDepth
	}

	if IsSSH(url) {
		err = ssh.AddToKnownHosts(url)
		if err != nil {
			return nil, fmt.Errorf("failed to add host to known_hosts: %w", err)
		}

		opts.URL = ConvertSSHUrl(url)
	} else if !IsLocalFile(url) {
		opts.InsecureSkipTLS = skipTLSVerify

		if proxyOpts != (transport.ProxyOptions{}) {
			opts.ProxyOptions = proxyOpts
		}
	}

	// Required for cloning from Azure DevOps repositories with go-git v5, should be fixed in v6
	// https://github.com/go-git/go-git/pull/613
	transport.UnsupportedCapabilities = []capability.Capability{
		capability.ThinPack,
	}

	repo, err := cloneWithRetry(path, opts)
	if err != nil && IsSSH(url) && ssh.IsHostKeyMismatchError(err) {
		if refreshErr := ssh.RefreshKnownHost(url); refreshErr != nil {
			return nil, fmt.Errorf("failed to refresh host key after mismatch: %w", refreshErr)
		}

		repo, err = cloneWithRetry(path, opts)
	}

	if err != nil {
		if errors.Is(err, transport.ErrInvalidAuthMethod) && cloneSubmodules {
			return nil, fmt.Errorf("%w: %w", err, ErrPossibleAuthMethodMismatch)
		}

		// Handle partial state: if remote already exists (race/previous attempt), try to recover
		if errors.Is(err, git.ErrRemoteExists) {
			// If the directory contains a repository, try UpdateRepository
			if _, openErr := git.PlainOpen(path); openErr == nil {
				if upd, uErr := UpdateRepository(path, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth); uErr == nil {
					return upd, nil
				}
			}

			// Remove path and retry clone once
			_ = os.RemoveAll(path)

			repo, err = cloneWithRetry(path, opts)
			if err != nil {
				return nil, fmt.Errorf("clone failed: %w", err)
			}
		} else {
			return nil, fmt.Errorf("clone failed: %w", err)
		}
	}

	err = CheckoutRepository(repo, ref, auth, cloneSubmodules)
	if err != nil {
		// Attempt to deepen if the ref is unreachable in a shallow clone
		if depth > 0 && IsRefUnreachableError(err) {
			repo, deepenErr := deepenAndCheckout(repo, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
			if deepenErr != nil {
				return nil, fmt.Errorf("%w: %w", ErrCheckoutFailed, deepenErr)
			}

			return repo, nil
		}

		return nil, fmt.Errorf("%w: %w", ErrCheckoutFailed, err)
	}

	return repo, err
}

// cloneWithRetry attempts to clone a repository with the provided options, retrying on transient errors.
func cloneWithRetry(path string, opts *git.CloneOptions) (*git.Repository, error) {
	var repo *git.Repository

	err := retrier.Do(
		func() error {
			var err error

			repo, err = git.PlainClone(path, false, opts)
			if err != nil && !errors.Is(err, git.ErrRemoteExists) {
				return err
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return repo, nil
}

func updateSubmodules(repo *git.Repository, auth transport.AuthMethod, depth int) error {
	worktree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	parentRemoteURL, err := getPrimaryRemoteURL(repo)
	if err != nil {
		return fmt.Errorf("failed to get parent repository remote URL: %w", err)
	}

	submodules, err := worktree.Submodules()
	if err != nil {
		return fmt.Errorf("failed to list submodules: %w", err)
	}

	for _, submodule := range submodules {
		slog.Debug("updating submodule",
			"name", submodule.Config().Name,
			"path", filepath.Join(worktree.Filesystem.Root(), submodule.Config().Path))

		submoduleRepo, err := submodule.Repository()
		if err != nil {
			// If the submodule isn't initialized, try to initialize it and retry
			if errors.Is(err, git.ErrSubmoduleNotInitialized) {
				if initErr := submodule.Init(); initErr != nil {
					return fmt.Errorf("failed to init submodule %s: %w", submodule.Config().Path, initErr)
				}

				submoduleRepo, err = submodule.Repository()
				if err != nil {
					return fmt.Errorf("failed to get submodule repository after init: %w", err)
				}
			} else {
				return fmt.Errorf("failed to get submodule repository: %w", err)
			}
		}

		// Reset tracked files in submodule
		err = ResetTrackedFiles(submoduleRepo)
		if err != nil {
			return fmt.Errorf("failed to reset tracked files in submodule: %w", err)
		}

		resolvedSubmoduleURL := submodule.Config().URL
		if isRelativeSubmoduleURL(resolvedSubmoduleURL) {
			resolvedSubmoduleURL, err = resolveSubmoduleURL(parentRemoteURL, resolvedSubmoduleURL)
			if err != nil {
				return fmt.Errorf("failed to resolve relative URL for submodule %s: %w", submodule.Config().Path, err)
			}

			err = updateRemoteURL(submoduleRepo, resolvedSubmoduleURL)
			if err != nil {
				return fmt.Errorf("failed to set resolved remote URL for submodule %s: %w", submodule.Config().Path, err)
			}
		}

		opts := &git.SubmoduleUpdateOptions{
			Init:              true,
			RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
			Auth:              auth,
			Depth:             depth,
		}

		if resolvedSubmoduleURL != "" {
			resolvedAuth, err := GetAuthMethod(resolvedSubmoduleURL, "", "", "")
			if err != nil {
				return fmt.Errorf("failed to resolve auth method for submodule %s: %w", submodule.Config().Path, err)
			}

			if resolvedAuth != nil {
				opts.Auth = resolvedAuth
			}
		}

		err = retrier.Do(
			func() error {
				if err = submodule.Update(opts); err != nil {
					submodulePath := "submodule"
					if cfg := submodule.Config(); cfg.Path != "" {
						submodulePath = cfg.Path
					}

					switch {
					case errors.Is(err, git.ErrUnstagedChanges):
						// Hard reset and try again
						submoduleRepoWorktree, err := submoduleRepo.Worktree()
						if err != nil {
							return fmt.Errorf("failed to get worktree for %s: %w", submodulePath, err)
						}

						err = submoduleRepoWorktree.Reset(&git.ResetOptions{
							Mode: git.HardReset,
						})
						if err != nil {
							return fmt.Errorf("failed to reset worktree for %s: %w", submodulePath, err)
						}

						// Retry submodule update
						err = submodule.Update(opts)
						if err != nil {
							return fmt.Errorf("failed to update %s after resetting: %w", submodulePath, err)
						}
					case errors.Is(err, transport.ErrInvalidAuthMethod):
						return fmt.Errorf("%w: %w", err, ErrPossibleAuthMethodMismatch)
					default:
						return fmt.Errorf("failed to update %s: %w", submodulePath, err)
					}
				}

				return nil
			})
		if err != nil {
			return err
		}
	}

	return nil
}

func getPrimaryRemoteURL(repo *git.Repository) (string, error) {
	remote, err := repo.Remote(RemoteName)
	if err != nil {
		return "", fmt.Errorf("failed to get remote %s: %w", RemoteName, err)
	}

	remoteConfig := remote.Config()
	if remoteConfig == nil || len(remoteConfig.URLs) == 0 || strings.TrimSpace(remoteConfig.URLs[0]) == "" {
		return "", fmt.Errorf("remote %s has no URL configured", RemoteName)
	}

	return remoteConfig.URLs[0], nil
}

func isRelativeSubmoduleURL(url string) bool {
	trimmed := strings.TrimSpace(url)
	if trimmed == "" {
		return false
	}

	if IsSSH(trimmed) || strings.Contains(trimmed, "://") || strings.HasPrefix(trimmed, "file://") {
		return false
	}

	if strings.HasPrefix(trimmed, "/") {
		return true
	}

	return strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, "../")
}

func resolveSubmoduleURL(parentRemoteURL, submoduleURL string) (string, error) {
	parent := strings.TrimSpace(parentRemoteURL)
	relative := strings.TrimSpace(submoduleURL)

	if parent == "" {
		return "", errors.New("parent remote URL is empty")
	}

	if relative == "" {
		return "", errors.New("submodule URL is empty")
	}

	if !isRelativeSubmoduleURL(relative) {
		return relative, nil
	}

	if IsSSH(parent) {
		parent = ConvertSSHUrl(parent)
	}

	endpoint, err := transport.NewEndpoint(parent)
	if err != nil {
		return "", fmt.Errorf("failed to parse parent remote URL %q: %w", parentRemoteURL, err)
	}

	if strings.HasPrefix(relative, "/") {
		endpoint.Path = path.Clean(relative)
	} else {
		endpoint.Path = path.Clean(path.Join(endpoint.Path, relative))
	}

	return endpoint.String(), nil
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

	r, err := repo.Reference(refSet.RemoteRef, true)
	if err != nil {
		return plumbing.ZeroHash.String(), fmt.Errorf("failed to get reference %s: %w", ref, err)
	}

	// Get the commit object for the reference
	commit, err := repo.CommitObject(r.Hash())
	if err != nil {
		return plumbing.ZeroHash.String(), fmt.Errorf("failed to get commit object for %s: %w", r.Hash(), err)
	}

	return commit.Hash.String(), nil
}

// ResetTrackedFiles resets all tracked files in the worktree To their last committed state
// while leaving untracked files intact.
func ResetTrackedFiles(repo *git.Repository) error {
	worktree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	repoRoot := worktree.Filesystem.Root()

	changedFiles, err := worktree.Status()
	if err != nil {
		return fmt.Errorf("failed to get worktree status: %w", err)
	}

	resetFiles := make([]string, 0, len(changedFiles))

	for file, status := range changedFiles {
		// Do not touch files that are not part of the Git repository (e.g. created by a container process)
		if status.Staging == git.Untracked {
			continue
		}

		if shouldResetDecryptedFile(repo, repoRoot, file) {
			resetFiles = append(resetFiles, file)
		}
	}

	if len(resetFiles) > 0 {
		err = worktree.Reset(&git.ResetOptions{
			Mode:  git.HardReset,
			Files: resetFiles,
		})
		if err != nil {
			return fmt.Errorf("failed to reset worktree: %w", err)
		}
	}

	return nil
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

// shouldResetDecryptedFile determines whether a file should be reset based on its decrypted content.
func shouldResetDecryptedFile(repo *git.Repository, repoRoot, file string) bool {
	headRef, err := repo.Head()
	if err != nil {
		return true
	}

	commit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return true
	}
	// Get file from commit tree
	fileObj, err := commit.File(file)
	if err != nil {
		return true // Not tracked, default to reset
	}

	committedBytes, err := fileObj.Contents()
	if err != nil {
		return true
	}

	format := encryption.GetFileFormat(fileObj.Name)

	decryptedContent, err := encryption.DecryptContent([]byte(committedBytes), format)
	if err != nil {
		return true
	}

	workingContent, err := os.ReadFile(filepath.Join(repoRoot, file)) // #nosec G304
	if err != nil {
		return true
	}

	return !strings.EqualFold(string(decryptedContent), string(workingContent))
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

	iter, err := repo.CommitObjects()
	if err != nil {
		return "", err
	}
	defer iter.Close()

	// collect all commit SHAs, skipping any errors
	var (
		allSHAs     []string
		foundCommit bool
	)

	err = iter.ForEach(func(c *object.Commit) error {
		if c == nil {
			return nil
		}

		sha := c.Hash.String()

		allSHAs = append(allSHAs, sha)
		if sha == commitSHA {
			foundCommit = true
		}

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("error iterating commits: %w", err)
	}

	if !foundCommit {
		return "", fmt.Errorf("commit SHA %s not found in repository", commitSHA)
	}

	shaLen := len(commitSHA)
	for length := minLength; length <= shaLen; length++ {
		prefixCount := make(map[string]int, len(allSHAs))
		for _, sha := range allSHAs {
			if len(sha) >= length {
				prefix := sha[:length]
				prefixCount[prefix]++
			}
		}

		prefix := commitSHA[:length]
		if prefixCount[prefix] == 1 {
			return prefix, nil
		}
	}

	return "", fmt.Errorf("no unique prefix found for commit SHA %s", commitSHA)
}

// GetRepoName returns the repository name in the form "<host>/<owner>/<repo>" from the given clone URL.
// Supports:
//   - https://github.com/owner/repo(.git)
//   - http://github.com/owner/repo(.git)
//   - ssh://github.com/owner/repo(.git)
//   - git@github.com:owner/repo(.git)
//   - token-injected https like https://oauth2:TOKEN@github.com/owner/repo(.git)
func GetRepoName(cloneURL string) string {
	u := strings.TrimSpace(cloneURL)
	if u == "" {
		return ""
	}

	// Handle classic SCP-like SSH: git@host:owner/repo(.git)
	if strings.Contains(u, "@") && strings.Contains(u, ":") && !strings.Contains(u, "://") {
		parts := strings.SplitN(u, "@", 2)
		if len(parts) == 2 {
			hostAndPath := parts[1]

			hostParts := strings.SplitN(hostAndPath, ":", 2)
			if len(hostParts) == 2 {
				host := hostParts[0]
				repoPath := strings.TrimPrefix(hostParts[1], "/")
				ownerRepo := normalizeOwnerRepo(repoPath)

				return host + "/" + ownerRepo
			}
		}
	}

	// Local filesystem repositories: use the absolute path (minus leading slash) as the
	// name, mirroring the "host/owner/repo" hierarchy used for remote URLs so that two
	// different local paths never collide.
	if strings.HasPrefix(u, "file://") {
		parsed, err := url.Parse(u)
		if err == nil {
			p := strings.TrimPrefix(parsed.Path, "/")

			return normalizeOwnerRepo(p)
		}
	}

	// For URLs with a scheme use net/url
	parsed, err := url.Parse(u)
	if err == nil && parsed.Host != "" {
		p := strings.TrimPrefix(parsed.Path, "/")
		ownerRepo := normalizeOwnerRepo(p)

		return parsed.Host + "/" + ownerRepo
	}

	// Fallback: attempt to normalize directly
	return normalizeOwnerRepo(u)
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

	head, err := repo.Head()
	if err != nil {
		return false, fmt.Errorf("%w for repository '%s': %w", ErrGetHeadFailed, path, err)
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

// needsReclone returns true when the on-disk repository shallow state does not
// match the requested depth, indicating a transition (e.g. full→shallow or shallow→full).
func needsReclone(repoPath string, depth int) bool {
	shallowFile := filepath.Join(repoPath, ".git", "shallow")

	_, err := os.Stat(shallowFile)
	isShallow := err == nil

	wantShallow := depth > 0

	return isShallow != wantShallow
}

// IsRefUnreachableError returns true when the error indicates the requested ref
// could not be found, which in a shallow clone likely means the ref is outside
// the fetched depth.
func IsRefUnreachableError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, plumbing.ErrObjectNotFound) ||
		errors.Is(err, ErrInvalidReference) ||
		errors.Is(err, plumbing.ErrReferenceNotFound) {
		return true
	}

	// go-git may wrap the object-not-found message in other errors
	msg := err.Error()

	return strings.Contains(msg, "object not found") ||
		strings.Contains(msg, "reference not found")
}

// isNonRecoverableError returns true for auth, network, or proxy errors that
// should NOT trigger a deepen attempt.
func isNonRecoverableError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, transport.ErrAuthenticationRequired) ||
		errors.Is(err, transport.ErrAuthorizationFailed) ||
		errors.Is(err, transport.ErrInvalidAuthMethod) {
		return true
	}

	_, isURLErr := errors.AsType[*url.Error](err)
	netErr, isNetErr := errors.AsType[net.Error](err)

	return isURLErr || (isNetErr && netErr.Timeout())
}

// deepenAndCheckout incrementally deepens a shallow repository to resolve an
// unreachable ref. It tries depth×2, depth×4, then a full fetch (depth 0).
// It emits a warning log at each deepening step so operators understand why
// clone time increased.
func deepenAndCheckout(repo *git.Repository, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, cloneSubmodules bool, currentDepth int) (*git.Repository, error) {
	steps := []int{currentDepth * 2, currentDepth * 4, 0}

	for _, newDepth := range steps {
		label := strconv.Itoa(newDepth)
		if newDepth == 0 {
			label = "full (unlimited)"
		}

		slog.Warn("shallow fetch insufficient for requested ref, deepening",
			slog.String("ref", ref),
			slog.Int("previous_depth", currentDepth),
			slog.String("new_depth", label))

		err := FetchRepository(repo, url, skipTLSVerify, proxyOpts, auth, newDepth)
		if err != nil {
			if isNonRecoverableError(err) {
				return nil, fmt.Errorf("non-recoverable error during deepen: %w", err)
			}
			// Try the next deepening step
			continue
		}

		err = CheckoutRepository(repo, ref, auth, cloneSubmodules)
		if err == nil {
			return repo, nil
		}

		if isNonRecoverableError(err) {
			return nil, fmt.Errorf("non-recoverable error during checkout after deepen: %w", err)
		}

		// If still unreachable, try next step
		currentDepth = newDepth
	}

	return nil, fmt.Errorf("ref %s not reachable even after full fetch", ref)
}

// normalizeOwnerRepo cleans a path and returns "owner/repo" or empty string when not possible.
func normalizeOwnerRepo(p string) string {
	// Remove query or fragment if present in raw strings
	if idx := strings.IndexAny(p, "?#"); idx >= 0 {
		p = p[:idx]
	}

	// Trim trailing '.git'
	p = strings.TrimSuffix(p, ".git")

	// Clean path
	return path.Clean(p)
}

// FormatGitErrorMessage formats a git operation error message by attempting to decode
// any localized error response bodies embedded in it (e.g. from Azure DevOps Server).
// It returns the formatted error message suitable for logging.
func FormatGitErrorMessage(err error) string {
	if err == nil {
		return ""
	}

	return errdecode.DecodeEmbeddedJSON(err.Error())
}
