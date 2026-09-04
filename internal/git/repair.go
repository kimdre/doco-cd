package git

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/filesystem/dotgit"
)

// IsCorruptionError checks if an error indicates repository corruption rather than a transient failure.
func IsCorruptionError(err error) bool {
	if err == nil {
		return false
	}

	corruptionErrors := []error{
		plumbing.ErrReferenceNotFound,
		plumbing.ErrObjectNotFound,
		git.ErrInvalidReference,
		dotgit.ErrEmptyRefFile,
		dotgit.ErrPackedRefsBadFormat,
		dotgit.ErrPackedRefsDuplicatedRef,
		dotgit.ErrSymRefTargetNotFound,
	}

	if slices.ContainsFunc(corruptionErrors, func(target error) bool {
		return errors.Is(err, target)
	}) {
		return true
	}

	// patterns for common corruption-related error messages that may be wrapped by go-git
	patterns := []string{
		"reference not found",
		"object not found",
		"invalid reference",
	}

	// Check error message for corruption-related patterns
	msg := err.Error()

	return slices.ContainsFunc(patterns, func(pattern string) bool {
		return strings.Contains(msg, pattern)
	})
}

// remoteRefExists performs an ls-remote operation to check if a reference exists on the remote.
// Returns true if the ref exists on remote, false if not found, and error for transient issues.
func remoteRefExists(ctx context.Context, repo *git.Repository, url string, auth transport.AuthMethod, ref plumbing.ReferenceName) (bool, error) {
	_ = ctx
	_ = url

	remote, err := repo.Remote(RemoteName)
	if err != nil {
		return false, fmt.Errorf("failed to get remote %s: %w", RemoteName, err)
	}

	// Use ls-remote to check what refs exist on the remote
	refs, err := remote.List(&git.ListOptions{Auth: auth})
	if err != nil {
		return false, fmt.Errorf("failed to list remote refs: %w", err)
	}

	for _, remoteRef := range refs {
		if remoteRef.Name() == ref {
			return true, nil
		}
	}

	return false, nil
}

// repositoryMetadataValid verifies that HEAD and all references can be read.
func repositoryMetadataValid(repo *git.Repository) bool {
	head, err := repo.Head()
	if err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false
	}

	refs, err := repo.References()
	if err != nil {
		return false
	}
	defer refs.Close()

	refCount := 0

	err = refs.ForEach(func(_ *plumbing.Reference) error {
		refCount++
		return nil
	})
	if err != nil {
		return false
	}

	return refCount > 0 || head != nil
}

// RepairRepository attempts to fix a corrupted Git repository.
// It first tries lightweight repairs (validation), then falls back to re-cloning if needed.
// It emits warnings when corruption is detected and recovered.
// Returns the repaired repository or an error if repair fails.
func RepairRepository(
	path, url, ref string,
	skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod,
	cloneSubmodules bool, depth int,
	logger *slog.Logger,
) (*git.Repository, error) {
	depth = effectiveDepth(url, depth)

	unlock := AcquirePathLock(path)
	defer unlock()

	return repairRepositoryLocked(path, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth, logger)
}

// repairRepositoryLocked repairs a repository while the caller holds its path lock.
func repairRepositoryLocked(
	path, url, ref string,
	skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod,
	cloneSubmodules bool, depth int,
	logger *slog.Logger,
) (*git.Repository, error) {
	if logger == nil {
		logger = slog.Default()
	}

	logger.Warn("repository corruption detected, attempting recovery",
		slog.String("path", path),
		slog.String("url", url),
		slog.String("ref", ref))

	recloneReason := "repository metadata is unreadable"

	repo, openErr := git.PlainOpen(path)
	if openErr == nil && repositoryMetadataValid(repo) {
		fetchErr := fetchRepositoryLocked(repo, url, ref, skipTLSVerify, proxyOpts, auth, depth)
		switch {
		case fetchErr != nil:
			if !IsCorruptionError(fetchErr) {
				return nil, fmt.Errorf("in-place repair fetch failed: %w", fetchErr)
			}

			recloneReason = "in-place fetch failed: " + FormatGitErrorMessage(fetchErr)
		default:
			checkoutErr := CheckoutRepository(repo, ref, auth, cloneSubmodules, depth)
			if checkoutErr == nil {
				logger.Info("repository successfully repaired in place",
					slog.String("path", path))

				return repo, nil
			}

			if !IsCorruptionError(checkoutErr) {
				return nil, fmt.Errorf("in-place repair checkout failed: %w", checkoutErr)
			}

			recloneReason = "in-place checkout failed: " + FormatGitErrorMessage(checkoutErr)
		}
	} else if openErr != nil {
		recloneReason = "repository could not be opened: " + FormatGitErrorMessage(openErr)
	}

	logger.Warn("in-place repair failed, performing full repository re-clone",
		slog.String("path", path),
		slog.String("reason", recloneReason))

	if err := os.RemoveAll(path); err != nil {
		return nil, fmt.Errorf("failed to remove corrupted repository at %s: %w", path, err)
	}

	repairedRepo, err := cloneRepositoryLocked(path, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
	if err != nil {
		return nil, fmt.Errorf("failed to re-clone repository during repair: %w", err)
	}

	logger.Info("repository successfully repaired by re-cloning",
		slog.String("path", path),
		slog.String("url", url))

	return repairedRepo, nil
}
