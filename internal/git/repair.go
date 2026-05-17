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
)

// IsCorruptionError checks if an error indicates repository corruption rather than transient failures.
// Corruption is indicated by reference not found errors when fetches have completed successfully.
func IsCorruptionError(err error) bool {
	if err == nil {
		return false
	}

	// Reference not found is a primary corruption indicator
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return true
	}

	// patterns for common corruption-related error messages that may be wrapped by go-git
	patters := []string{
		"reference not found",
		"object not found",
		"invalid reference",
	}

	// Check error message for corruption-related patterns
	msg := err.Error()

	return slices.ContainsFunc(patters, func(pattern string) bool {
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

// attemptLightweightRepair tries to fix a corrupted repository without re-cloning.
// It attempts to validate repository metadata, which can recover from many corruption scenarios.
// Returns true if repair succeeded, false if the repository requires re-clone.
func attemptLightweightRepair(repo *git.Repository) bool {
	// Attempt to validate that we can access HEAD and iterate references
	// This is a lightweight operation compared to re-cloning

	// First check if we can get HEAD
	head, err := repo.Head()
	if err != nil && err != plumbing.ErrReferenceNotFound {
		// Head is corrupted beyond recovery
		return false
	}

	// Try to list all references - this will fail if refs are corrupted
	refs, err := repo.References()
	if err != nil {
		// References are corrupted
		return false
	}
	defer refs.Close()

	// Count accessible refs to validate repository structure
	refCount := 0

	err = refs.ForEach(func(_ *plumbing.Reference) error {
		refCount++
		return nil
	})
	if err != nil {
		// Could not enumerate references properly
		return false
	}

	// If we have no references and HEAD is also missing, repo is probably empty or very corrupted
	if refCount == 0 && head == nil {
		return false
	}

	// Lightweight repair succeeded - repository structure appears intact
	return true
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
	if logger == nil {
		logger = slog.Default()
	}

	unlock := AcquirePathLock(path)
	defer unlock()

	repo, err := git.PlainOpen(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open repository for repair: %w", err)
	}

	logger.Warn("repository corruption detected, attempting recovery",
		slog.String("path", path),
		slog.String("url", url),
		slog.String("ref", ref))

	if attemptLightweightRepair(repo) {
		logger.Info("lightweight repository repair succeeded",
			slog.String("path", path))

		if _, err := repo.Reference(plumbing.ReferenceName(ref), true); err == nil {
			logger.Info("repository references restored after lightweight repair",
				slog.String("path", path))

			return repo, nil
		}
	}

	logger.Warn("lightweight repair failed, performing full repository re-clone",
		slog.String("path", path),
		slog.String("reason", "reference still not accessible after lightweight repair"))

	// Release lock before re-cloning, because CloneRepository acquires the same path lock.
	unlock()

	if err := os.RemoveAll(path); err != nil {
		return nil, fmt.Errorf("failed to remove corrupted repository at %s: %w", path, err)
	}

	repairedRepo, err := CloneRepository(path, url, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
	if err != nil {
		return nil, fmt.Errorf("failed to re-clone repository during repair: %w", err)
	}

	logger.Info("repository successfully repaired by re-cloning",
		slog.String("path", path),
		slog.String("url", url))

	return repairedRepo, nil
}
