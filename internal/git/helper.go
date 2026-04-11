package git

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

func CloneOrUpdateRepository(log *slog.Logger,
	cloneUrl string, ref string, internalRepoPath, externalRepoPath string,
	private bool, sshPrivateKey string, sshPrivateKeyPassphrase string, gitAccessToken string,
	skipTLSVerify bool, proxyOpts transport.ProxyOptions, cloneSubmodules bool,
) (*git.Repository, error) {
	// Clone the repository
	log.Debug("cloning repository",
		slog.String("url", cloneUrl),
		slog.String("reference", ref),
		slog.String("container_path", internalRepoPath),
		slog.String("host_path", externalRepoPath))

	auth, err := GetAuthMethod(cloneUrl, sshPrivateKey, sshPrivateKeyPassphrase, gitAccessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to get auth method: %w", err)
	}

	if auth == nil && private {
		return nil, ErrMissingAuthToken
	}

	var repo *git.Repository
	// Try to clone the repository
	repo, err = CloneRepository(internalRepoPath, cloneUrl, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules)
	if err != nil {
		// If the repository already exists, check it out to the specified commit SHA
		if errors.Is(err, ErrRepositoryAlreadyExists) {
			log.Debug("repository already exists, checking out reference "+ref, slog.String("host_path", externalRepoPath))

			repo, err = UpdateRepository(internalRepoPath, cloneUrl, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	} else {
		log.Debug("repository cloned", slog.String("path", externalRepoPath))
	}

	return repo, nil
}
