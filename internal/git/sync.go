package git

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/git/ssh"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
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

	unlock := sourcecache.AcquirePathLock(worktree.Filesystem.Root())
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

	unlock := sourcecache.AcquirePathLock(worktree.Filesystem.Root())
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
	// A one-level name that is safe on its own is a pseudo-ref such as HEAD or
	// FETCH_HEAD, which cannot be resolved to a single branch or tag refspec.
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
