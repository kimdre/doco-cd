package git

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git/ssh"
)

func init() {
	// Required for cloning from Azure DevOps repositories with go-git v5.
	// Configure the package-level transport setting once to keep clones race-free.
	transport.UnsupportedCapabilities = []capability.Capability{
		capability.ThinPack,
	}
}

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

// effectiveDepth returns the usable clone/fetch depth for a URL.
// Local file:// repositories are always fetched in full: the in-process transport
// used for them (see local_transport.go) does not implement git's shallow capability,
// so any depth > 0 would fail the transfer outright.
func effectiveDepth(url string, depth int) int {
	if depth > 0 && IsLocalFile(url) {
		slog.Debug("ignoring shallow depth for local filesystem repository",
			slog.String("url", url),
			slog.Int("requested_depth", depth))

		return 0
	}

	return depth
}

// FetchRepository fetches updates from the remote repository, including all branches and tags, and prunes deleted references.
// If depth > 0, a shallow fetch is performed with the specified number of commits.
func FetchRepository(repo *git.Repository, url string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, depth int) error {
	worktree, err := repo.Worktree()
	if err != nil {
		// Bare repositories have no worktree path to use as a lock key.
		return fetchRepositoryLocked(repo, url, "", skipTLSVerify, proxyOpts, auth, depth)
	}

	unlock := AcquirePathLock(worktree.Filesystem.Root())
	defer unlock()

	return fetchRepositoryLocked(repo, url, "", skipTLSVerify, proxyOpts, auth, depth)
}

// FetchRepositoryReference fetches the requested reference using focused refspecs
// where possible and falls back to the compatibility all-refs transfer as needed.
func FetchRepositoryReference(repo *git.Repository, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, depth int) error {
	worktree, err := repo.Worktree()
	if err != nil {
		return fetchRepositoryLocked(repo, url, ref, skipTLSVerify, proxyOpts, auth, depth)
	}

	unlock := AcquirePathLock(worktree.Filesystem.Root())
	defer unlock()

	return fetchRepositoryLocked(repo, url, ref, skipTLSVerify, proxyOpts, auth, depth)
}

// fetchRepositoryLocked fetches a repository while the caller holds its path lock.
func fetchRepositoryLocked(repo *git.Repository, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, depth int) error {
	depth = effectiveDepth(url, depth)

	newFetchOptions := func(refSpecs []config.RefSpec, tags git.TagMode) *git.FetchOptions {
		opts := &git.FetchOptions{
			RemoteName: RemoteName,
			RemoteURL:  url,
			RefSpecs:   refSpecs,
			Prune:      true,
			Auth:       auth,
			Depth:      depth,
			Tags:       tags,
		}

		if IsSSH(url) {
			opts.RemoteURL = ConvertSSHUrl(url)
		} else if !IsLocalFile(url) {
			opts.InsecureSkipTLS = skipTLSVerify

			if proxyOpts != (transport.ProxyOptions{}) {
				opts.ProxyOptions = proxyOpts
			}
		}

		return opts
	}

	// SSH auth when key is provided
	if IsSSH(url) {
		err := ssh.AddToKnownHosts(url)
		if err != nil {
			return fmt.Errorf("failed to add host to known_hosts: %w", err)
		}
	}

	fetchWithRetry := func(opts *git.FetchOptions) error {
		return retrier.Do(
			func() error {
				err := repo.Fetch(opts)
				if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
					return err
				}

				return nil
			})
	}

	fetch := func(opts *git.FetchOptions) error {
		err := fetchWithRetry(opts)
		if err != nil && IsSSH(url) && ssh.IsHostKeyMismatchError(err) {
			if refreshErr := ssh.RefreshKnownHost(url); refreshErr != nil {
				if !errors.Is(refreshErr, ssh.ErrKnownHostsUserManaged) {
					return fmt.Errorf("failed to refresh host key after mismatch: %w", refreshErr)
				}
			} else {
				err = fetchWithRetry(opts)
			}
		}

		return err
	}

	if ref == "" {
		return fetch(newFetchOptions([]config.RefSpec{refSpecAllBranches, refSpecAllTags}, git.AllTags))
	}

	var focusedErr error

	for _, refSpecs := range focusedFetchRefSpecs(ref) {
		destination, destinationErr := focusedFetchDestination(refSpecs)
		if destinationErr != nil {
			return destinationErr
		}

		err := fetch(newFetchOptions(refSpecs, git.NoTags))
		if err != nil {
			if !isFocusedFetchFallbackError(err) {
				return err
			}

			// go-git rejects a refspec whose source is missing on the remote before
			// it prunes anything, so the stale destination is removed here. Leaving
			// it in place would shadow the reference that still exists remotely.
			if errors.Is(err, git.NoMatchingRefSpecError{}) {
				if pruneErr := removeReferenceIfExists(repo, destination); pruneErr != nil {
					return fmt.Errorf("failed to prune stale reference %s: %w", destination, pruneErr)
				}
			}

			focusedErr = errors.Join(focusedErr, err)

			continue
		}

		exists, existsErr := referenceExists(repo, destination)
		if existsErr != nil {
			return fmt.Errorf("failed to validate focused fetch for %s: %w", ref, existsErr)
		}

		if exists {
			return nil
		}

		focusedErr = errors.Join(focusedErr, fmt.Errorf("focused fetch did not make reference %s reachable", ref))
	}

	broadErr := fetch(newFetchOptions([]config.RefSpec{refSpecAllBranches, refSpecAllTags}, git.AllTags))
	if broadErr != nil {
		if focusedErr != nil {
			return errors.Join(focusedErr, fmt.Errorf("compatibility fetch failed: %w", broadErr))
		}

		return broadErr
	}

	return nil
}

// focusedFetchDestination returns the destination reference a focused refspec writes to.
func focusedFetchDestination(refSpecs []config.RefSpec) (plumbing.ReferenceName, error) {
	if len(refSpecs) != 1 {
		return "", fmt.Errorf("expected exactly one focused refspec, got %d", len(refSpecs))
	}

	_, destination, ok := strings.Cut(string(refSpecs[0]), ":")
	if !ok {
		return "", fmt.Errorf("focused refspec has no destination: %s", refSpecs[0])
	}

	name := plumbing.ReferenceName(strings.TrimPrefix(destination, "+"))
	if !name.IsSafe() {
		return "", fmt.Errorf("focused refspec has unsafe destination: %s", refSpecs[0])
	}

	return name, nil
}

// referenceExists reports whether name is present in the repository.
func referenceExists(repo *git.Repository, name plumbing.ReferenceName) (bool, error) {
	_, err := repo.Reference(name, false)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	return true, nil
}

// removeReferenceIfExists deletes name when it is still present in the repository.
func removeReferenceIfExists(repo *git.Repository, name plumbing.ReferenceName) error {
	exists, err := referenceExists(repo, name)
	if err != nil || !exists {
		return err
	}

	return repo.Storer.RemoveReference(name)
}

// isFocusedFetchFallbackError determines if a fetch error should trigger a fallback to the compatibility all-refs fetch.
func isFocusedFetchFallbackError(err error) bool {
	return err != nil && !isNonRecoverableError(err) &&
		!errors.Is(err, transport.ErrRepositoryNotFound) &&
		!errors.Is(err, transport.ErrEmptyRemoteRepository)
}

// focusedFetchRefSpecs returns the branch-first attempts needed to fetch ref.
// Unknown, pseudo, and commit references intentionally return no focused plans so
// the caller uses the compatibility all-refs fetch.
func focusedFetchRefSpecs(ref string) [][]config.RefSpec {
	switch {
	case strings.HasPrefix(ref, BranchPrefix):
		name := strings.TrimPrefix(ref, BranchPrefix)
		if plumbing.NewBranchReferenceName(name).IsSafe() {
			return [][]config.RefSpec{{config.RefSpec(fmt.Sprintf(refSpecSingleBranch, name, name))}}
		}
	case strings.HasPrefix(ref, TagPrefix):
		name := strings.TrimPrefix(ref, TagPrefix)
		if plumbing.NewTagReferenceName(name).IsSafe() {
			return [][]config.RefSpec{{config.RefSpec(fmt.Sprintf(refSpecSingleTag, name, name))}}
		}
	case !strings.HasPrefix(ref, "refs/") && !plumbing.IsHash(ref) && !plumbing.ReferenceName(ref).IsSafe():
		branch := plumbing.NewBranchReferenceName(ref)

		tag := plumbing.NewTagReferenceName(ref)
		if branch.IsSafe() && tag.IsSafe() {
			return [][]config.RefSpec{
				{config.RefSpec(fmt.Sprintf(refSpecSingleBranch, ref, ref))},
				{config.RefSpec(fmt.Sprintf(refSpecSingleTag, ref, ref))},
			}
		}
	}

	return nil
}

// SyncRepository opens an existing repository or creates it, then brings it to ref
// while holding the path lock for the full operation.
func SyncRepository(path, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, cloneSubmodules bool, depth int) (*SyncResult, error) {
	depth = effectiveDepth(url, depth)

	unlock := AcquirePathLock(path)
	defer unlock()

	repo, err := git.PlainOpen(path)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		repo, err = cloneRepositoryForSyncLocked(path, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
		if err != nil {
			return nil, err
		}

		return &SyncResult{Repository: repo, State: SyncStateCloned}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}

	before, beforeErr := repo.Head()
	recloned := needsReclone(path, depth)

	repo, err = updateRepositoryLocked(path, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
	if err != nil {
		return nil, err
	}

	after, afterErr := repo.Head()
	if !recloned && beforeErr == nil && afterErr == nil && before.Hash() == after.Hash() {
		return &SyncResult{Repository: repo, State: SyncStateCurrent}, nil
	}

	return &SyncResult{Repository: repo, State: SyncStateUpdated}, nil
}

// UpdateRepository fetches and checks out the requested ref.
// If depth > 0, shallow fetches are used. When the requested ref is not reachable
// within the current depth, the repository is incrementally deepened before falling
// back to a full fetch.
func UpdateRepository(path, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, cloneSubmodules bool, depth int) (*git.Repository, error) {
	depth = effectiveDepth(url, depth)

	// Serialize operations on the same path
	unlock := AcquirePathLock(path)
	defer unlock()

	return updateRepositoryLocked(path, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
}

// updateRepositoryLocked updates a repository while the caller holds its path lock.
func updateRepositoryLocked(path, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, cloneSubmodules bool, depth int) (*git.Repository, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return nil, err
	}

	// Detect shallow/full transition and re-clone if needed
	if needsReclone(path, depth) {
		slog.Info("git depth configuration changed, re-cloning repository",
			slog.String("path", path),
			slog.Int("requested_depth", depth))

		if err := os.RemoveAll(path); err != nil {
			return nil, fmt.Errorf("failed to remove repository for re-clone: %w", err)
		}

		return cloneRepositoryForSyncLocked(path, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
	}

	err = updateRemoteURL(repo, url)
	if err != nil {
		return nil, err
	}

	err = fetchRepositoryLocked(repo, url, ref, skipTLSVerify, proxyOpts, auth, depth)
	if err != nil {
		if IsCorruptionError(err) {
			repairedRepo, repairErr := repairAfterCorruptionLocked(path, url, ref, "fetch", err, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
			if repairErr == nil {
				return repairedRepo, nil
			}

			return nil, fmt.Errorf("%w: %w", ErrFetchFailed, errors.Join(err, fmt.Errorf("repair failed: %w", repairErr)))
		}

		return nil, fmt.Errorf("%w: %w", ErrFetchFailed, err)
	}

	fetchedExists, fetchedCheckErr := fetchedReferenceExistsAfterFetch(repo, ref)
	if fetchedCheckErr == nil && !fetchedExists {
		return nil, fmt.Errorf("%w: %w: %s", ErrCheckoutFailed, ErrInvalidReference, ref)
	}

	matches, matchesErr := repositoryMatchesHead(repo, ref)
	if matchesErr == nil && matches {
		return repo, nil
	}

	// Pass auth and cloneSubmodules so CheckoutRepository can ensure submodules are updated when needed.
	err = CheckoutRepository(repo, ref, auth, cloneSubmodules, depth)
	if err != nil {
		var corruptionRepairErr error

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

			repairedRepo, repairErr := repairAfterCorruptionLocked(path, url, ref, "checkout", err, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
			if repairErr == nil {
				return repairedRepo, nil
			}

			corruptionRepairErr = repairErr
			if depth <= 0 || !IsRefUnreachableError(err) {
				return nil, fmt.Errorf("%w: %w", ErrCheckoutFailed, errors.Join(err, fmt.Errorf("repair failed: %w", repairErr)))
			}

			reopenedRepo, reopenErr := git.PlainOpen(path)
			if reopenErr != nil {
				return nil, fmt.Errorf("%w: %w", ErrCheckoutFailed, errors.Join(
					fmt.Errorf("repair failed: %w", repairErr),
					fmt.Errorf("failed to reopen repository for deepening: %w", reopenErr),
				))
			}

			repo = reopenedRepo
		}
		// Attempt to deepen if the ref is unreachable in a shallow clone
		if depth > 0 && IsRefUnreachableError(err) {
			repo, deepenErr := deepenAndCheckout(repo, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
			if deepenErr != nil {
				if corruptionRepairErr != nil {
					deepenErr = errors.Join(deepenErr, fmt.Errorf("repair failed: %w", corruptionRepairErr))
				}

				return nil, fmt.Errorf("%w: %w", ErrCheckoutFailed, deepenErr)
			}

			return repo, nil
		}

		return nil, fmt.Errorf("%w: %w", ErrCheckoutFailed, err)
	}

	return repo, nil
}

// repairAfterCorruptionLocked attempts to repair a repository after detecting possible corruption during an operation.
func repairAfterCorruptionLocked(
	path, url, ref, operation string,
	cause error,
	skipTLSVerify bool,
	proxyOpts transport.ProxyOptions,
	auth transport.AuthMethod,
	cloneSubmodules bool,
	depth int,
) (*git.Repository, error) {
	slog.Warn("detected possible repository corruption during "+operation,
		slog.String("path", path),
		slog.String("error", FormatGitErrorMessage(cause)))

	repairedRepo, repairErr := repairRepositoryLocked(path, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth, slog.Default())
	if repairErr != nil {
		slog.Error("failed to repair corrupted repository",
			slog.String("path", path),
			slog.String("repair_error", FormatGitErrorMessage(repairErr)))
	}

	return repairedRepo, repairErr
}

// CloneRepository clones a repository with HTTP or SSH auth.
// If depth > 0, a shallow clone is performed with the specified number of commits.
func CloneRepository(path, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, cloneSubmodules bool, depth int) (*git.Repository, error) {
	depth = effectiveDepth(url, depth)

	// Serialize operations on the same path to avoid concurrent partial clones
	unlock := AcquirePathLock(path)
	defer unlock()

	return cloneRepositoryLocked(path, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
}

// cloneRepositoryLocked clones a repository while the caller holds its path lock.
func cloneRepositoryLocked(path, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, cloneSubmodules bool, depth int) (*git.Repository, error) {
	return cloneRepositoryWithReferenceLocked(path, url, ref, "", false, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
}

// cloneRepositoryForSyncLocked attempts to clone a repository using focused refspecs for the requested ref,
// falling back to a full clone if needed.
func cloneRepositoryForSyncLocked(path, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, cloneSubmodules bool, depth int) (*git.Repository, error) {
	var focusedErr error

	for _, refSpecs := range focusedFetchRefSpecs(ref) {
		source, _, ok := strings.Cut(string(refSpecs[0]), ":")
		if !ok {
			return nil, fmt.Errorf("focused clone refspec has no source: %s", refSpecs[0])
		}

		source = strings.TrimPrefix(source, "+")

		repo, err := cloneRepositoryWithReferenceLocked(
			path, url, ref, plumbing.ReferenceName(source), true,
			skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth,
		)
		if err == nil {
			return repo, nil
		}

		if !isFocusedFetchFallbackError(err) {
			return nil, err
		}

		focusedErr = errors.Join(focusedErr, err)
		if removeErr := os.RemoveAll(path); removeErr != nil {
			return nil, errors.Join(focusedErr, fmt.Errorf("failed to clean up focused clone: %w", removeErr))
		}
	}

	repo, err := cloneRepositoryLocked(path, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
	if err != nil {
		return nil, err
	}

	return repo, nil
}

// cloneRepositoryWithReferenceLocked clones a repository with an optional reference name for single-branch clones.
func cloneRepositoryWithReferenceLocked(
	path, url, ref string,
	referenceName plumbing.ReferenceName,
	singleBranch bool,
	skipTLSVerify bool,
	proxyOpts transport.ProxyOptions,
	auth transport.AuthMethod,
	cloneSubmodules bool,
	depth int,
) (*git.Repository, error) {
	err := os.MkdirAll(path, filesystem.PermDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", path, err)
	}

	opts := &git.CloneOptions{
		RemoteName: RemoteName,
		URL:        url,
		NoCheckout: true,
		Tags:       git.AllTags,
		Auth:       auth,
		Depth:      depth,
	}
	if singleBranch {
		opts.ReferenceName = referenceName
		opts.SingleBranch = true
		opts.Tags = git.NoTags
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

	repo, err := cloneWithRetry(path, opts)
	if err != nil && IsSSH(url) && ssh.IsHostKeyMismatchError(err) {
		if refreshErr := ssh.RefreshKnownHost(url); refreshErr != nil {
			if !errors.Is(refreshErr, ssh.ErrKnownHostsUserManaged) {
				return nil, fmt.Errorf("failed to refresh host key after mismatch: %w", refreshErr)
			}
		} else {
			repo, err = cloneWithRetry(path, opts)
		}
	}

	if err != nil {
		if errors.Is(err, transport.ErrInvalidAuthMethod) && cloneSubmodules {
			return nil, fmt.Errorf("%w: %w", err, ErrPossibleAuthMethodMismatch)
		}

		// Handle partial state: if remote already exists (race/previous attempt), try to recover
		if errors.Is(err, git.ErrRemoteExists) {
			// If the directory contains a repository, try UpdateRepository
			if _, openErr := git.PlainOpen(path); openErr == nil {
				if upd, uErr := updateRepositoryLocked(path, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth); uErr == nil {
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

	err = CheckoutRepository(repo, ref, auth, cloneSubmodules, depth)
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

// RepositoryNeedsReclone reports whether the repository depth state differs from
// the effective depth requested for the remote.
func RepositoryNeedsReclone(repoPath, url string, depth int) bool {
	return needsReclone(repoPath, effectiveDepth(url, depth))
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

// needsReclone returns true when the on-disk repository shallow state does not
// match the requested depth, indicating a transition (e.g. full→shallow or shallow→full).
func needsReclone(repoPath string, depth int) bool {
	shallowFile := filepath.Join(repoPath, ".git", "shallow")

	_, err := os.Stat(shallowFile)
	isShallow := err == nil

	wantShallow := depth > 0

	return isShallow != wantShallow
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

		err := fetchRepositoryLocked(repo, url, ref, skipTLSVerify, proxyOpts, auth, newDepth)
		if err != nil {
			if isNonRecoverableError(err) {
				return nil, fmt.Errorf("non-recoverable error during deepen: %w", err)
			}
			// Try the next deepening step
			continue
		}

		err = CheckoutRepository(repo, ref, auth, cloneSubmodules, newDepth)
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
