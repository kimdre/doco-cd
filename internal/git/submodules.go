package git

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

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
