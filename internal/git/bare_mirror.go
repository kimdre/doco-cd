package git

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git/ssh"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

// CloneOrUpdateBareMirror ensures a bare, checkout-free mirror of cloneURL
// exists at path and that ref is fetched into it, then returns the opened repository.
//
// It backs store.GitStore's mirror clone. This mirror never checks anything
// out: GitStore reads tree objects directly (ExportTree) and materializes
// submodules itself from tree objects, so the mirror only ever needs fetched objects and refs.
func CloneOrUpdateBareMirror(
	log *slog.Logger,
	cloneURL, ref, path string,
	private bool,
	sshPrivateKey, sshPrivateKeyPassphrase, gitAccessToken string,
	skipTLSVerify bool, proxyOpts transport.ProxyOptions,
	depth int,
) (*git.Repository, error) {
	if log == nil {
		log = slog.Default()
	}

	auth, err := GetAuthMethod(cloneURL, sshPrivateKey, sshPrivateKeyPassphrase, gitAccessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to get auth method: %w", err)
	}

	if auth == nil && private {
		return nil, ErrMissingAuthToken
	}

	depth = effectiveDepth(cloneURL, depth)

	unlock := sourcecache.AcquireExclusivePathLock(path)
	defer unlock()

	repo, err := git.PlainOpen(path)

	switch {
	case errors.Is(err, git.ErrRepositoryNotExists):
		if legacyCheckoutExists(path) {
			// A legacy .git checkout still sits next to the mirror path: startup
			// migration deferred its bootstrap (e.g. a running container still
			// references the old layout). Cloning a fresh, independent mirror here
			// would silently coexist with the un-migrated legacy content instead
			// of replacing it, duplicating history and artifacts. Fail clearly and
			// retryably instead; migration retries this repository on the next
			// doco-cd restart or once the legacy container stops.
			return nil, fmt.Errorf("%w: %s", ErrLegacyCheckoutNotMigrated, filepath.Dir(path))
		}

		log.Debug("cloning bare mirror",
			slog.String("url", cloneURL),
			slog.String("reference", ref),
			slog.String("path", path))

		return cloneBareMirrorLocked(path, cloneURL, ref, skipTLSVerify, proxyOpts, auth, depth)
	case err != nil:
		return nil, fmt.Errorf("failed to open bare mirror at %s: %w", path, err)
	}

	if bareMirrorNeedsReclone(path, depth) {
		log.Info("git depth configuration changed, re-cloning bare mirror",
			slog.String("path", path),
			slog.Int("requested_depth", depth))

		if err := os.RemoveAll(filepath.Clean(path)); err != nil {
			return nil, fmt.Errorf("failed to remove bare mirror for re-clone: %w", err)
		}

		return cloneBareMirrorLocked(path, cloneURL, ref, skipTLSVerify, proxyOpts, auth, depth)
	}

	if err := updateRemoteURL(repo, cloneURL); err != nil {
		return nil, err
	}

	if err := fetchRepositoryLocked(repo, cloneURL, ref, skipTLSVerify, proxyOpts, auth, depth); err != nil {
		if IsCorruptionError(err) {
			return repairBareMirrorLocked(path, cloneURL, ref, skipTLSVerify, proxyOpts, auth, depth, log)
		}

		return nil, fmt.Errorf("%w: %w", ErrFetchFailed, err)
	}

	exists, existsErr := fetchedReferenceExistsAfterFetch(repo, ref)
	if existsErr == nil && !exists {
		if depth > 0 {
			if deepenErr := deepenBareMirror(repo, cloneURL, ref, skipTLSVerify, proxyOpts, auth, depth); deepenErr != nil {
				return nil, deepenErr
			}

			return repo, nil
		}

		return nil, fmt.Errorf("%w: %w: %s", ErrFetchFailed, ErrInvalidReference, ref)
	}

	return repo, nil
}

// legacyCheckoutExists reports whether mirrorPath's parent directory still
// holds a legacy, non-bare ".git" checkout. mirrorPath is expected to be
// "<repoDir>/mirror"; its parent is the repository directory the pre-store-layout
// checkout used directly.
func legacyCheckoutExists(mirrorPath string) bool {
	_, err := git.PlainOpen(filepath.Join(filepath.Dir(mirrorPath), ".git"))
	return err == nil
}

// cloneBareMirrorLocked clones a bare mirror while the caller holds path's lock.
func cloneBareMirrorLocked(path, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, depth int) (*git.Repository, error) {
	path = filepath.Clean(path)

	if err := os.MkdirAll(path, filesystem.PermDir); err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", path, err)
	}

	opts := &git.CloneOptions{
		RemoteName: RemoteName,
		URL:        url,
		Tags:       git.AllTags,
		Auth:       auth,
		Depth:      depth,
	}

	if IsSSH(url) {
		if err := ssh.AddToKnownHosts(url); err != nil {
			return nil, fmt.Errorf("failed to add host to known_hosts: %w", err)
		}

		opts.URL = ConvertSSHUrl(url)
	} else if !IsLocalFile(url) {
		opts.InsecureSkipTLS = skipTLSVerify

		if proxyOpts != (transport.ProxyOptions{}) {
			opts.ProxyOptions = proxyOpts
		}
	}

	repo, err := cloneBareWithRetry(path, opts)
	if err != nil && IsSSH(url) && ssh.IsHostKeyMismatchError(err) {
		if refreshErr := ssh.RefreshKnownHost(url); refreshErr != nil {
			if !errors.Is(refreshErr, ssh.ErrKnownHostsUserManaged) {
				return nil, fmt.Errorf("failed to refresh host key after mismatch: %w", refreshErr)
			}
		} else {
			repo, err = cloneBareWithRetry(path, opts)
		}
	}

	// A leftover partial clone (e.g. from a prior crashed run) can leave the
	// remote already configured. The path lock rules out a concurrent clone
	// in this process, so it is safe to discard it and retry once.
	if err != nil && errors.Is(err, git.ErrRemoteExists) {
		if removeErr := os.RemoveAll(path); removeErr != nil {
			return nil, fmt.Errorf("failed to clean up partial bare mirror: %w", removeErr)
		}

		repo, err = cloneBareWithRetry(path, opts)
	}

	if err != nil {
		return nil, fmt.Errorf("clone failed: %w", err)
	}

	exists, existsErr := fetchedReferenceExistsAfterFetch(repo, ref)
	if existsErr == nil && !exists {
		if depth > 0 {
			if deepenErr := deepenBareMirror(repo, url, ref, skipTLSVerify, proxyOpts, auth, depth); deepenErr != nil {
				return nil, deepenErr
			}

			return repo, nil
		}

		return nil, fmt.Errorf("%w: %w: %s", ErrFetchFailed, ErrInvalidReference, ref)
	}

	return repo, nil
}

// cloneBareWithRetry attempts a bare clone, retrying on transient errors.
func cloneBareWithRetry(path string, opts *git.CloneOptions) (*git.Repository, error) {
	var repo *git.Repository

	err := retrier.Do(func() error {
		var err error

		repo, err = git.PlainClone(path, true, opts)

		return err
	})
	if err != nil {
		return nil, err
	}

	return repo, nil
}

// repairBareMirrorLocked recovers from a corrupted bare mirror by re-cloning
// it. Unlike repairRepositoryLocked, it never attempts an in-place checkout repair:
// bare mirrors have nothing to check out, so a successful fetch is
// already the strongest in-place repair signal available; if the fetch
// itself reported corruption, re-cloning is the only remaining option.
func repairBareMirrorLocked(path, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, depth int, log *slog.Logger) (*git.Repository, error) {
	log.Warn("bare mirror corruption detected, re-cloning",
		slog.String("path", path),
		slog.String("url", url))

	if err := os.RemoveAll(filepath.Clean(path)); err != nil {
		return nil, fmt.Errorf("failed to remove corrupted bare mirror at %s: %w", path, err)
	}

	repo, err := cloneBareMirrorLocked(path, url, ref, skipTLSVerify, proxyOpts, auth, depth)
	if err != nil {
		return nil, fmt.Errorf("failed to re-clone bare mirror during repair: %w", err)
	}

	return repo, nil
}

// deepenBareMirror incrementally deepens a shallow bare mirror to resolve an
// unreachable ref, trying depth×2, depth×4, then a full fetch (depth 0).
// It mirrors deepenAndCheckout's step sequence without the checkout step.
func deepenBareMirror(repo *git.Repository, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, currentDepth int) error {
	steps := []int{currentDepth * 2, currentDepth * 4, 0}

	for _, newDepth := range steps {
		if err := fetchRepositoryLocked(repo, url, ref, skipTLSVerify, proxyOpts, auth, newDepth); err != nil {
			if isNonRecoverableError(err) {
				return fmt.Errorf("non-recoverable error during deepen: %w", err)
			}

			continue
		}

		exists, existsErr := fetchedReferenceExistsAfterFetch(repo, ref)
		if existsErr == nil && exists {
			return nil
		}
	}

	return fmt.Errorf("%w: reference %s not reachable even after full fetch", ErrInvalidReference, ref)
}

// bareMirrorNeedsReclone reports whether the on-disk bare mirror's shallow
// state does not match the requested depth. It mirrors needsReclone, but a
// bare repository's git-dir *is* its root - there is no ".git" subdirectory
// to look inside.
func bareMirrorNeedsReclone(repoPath string, depth int) bool {
	repoPath = filepath.Clean(repoPath)
	shallowFile := filepath.Join(repoPath, "shallow")

	_, err := os.Stat(shallowFile)
	isShallow := err == nil

	wantShallow := depth > 0

	return isShallow != wantShallow
}
