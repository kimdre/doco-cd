package git

import (
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

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
