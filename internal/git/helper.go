package git

import (
	"fmt"
	"log/slog"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

func CloneOrUpdateRepository(log *slog.Logger,
	cloneUrl string, ref string, internalRepoPath, externalRepoPath string,
	private bool, sshPrivateKey string, sshPrivateKeyPassphrase string, gitAccessToken string,
	skipTLSVerify bool, proxyOpts transport.ProxyOptions, cloneSubmodules bool, depth int,
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

	if auth != nil {
		log.Debug("Using auth method",
			slog.String("name", auth.Name()),
		)
	} else {
		log.Debug("No auth method configured, using anonymous access")
	}

	syncResult, err := SyncRepository(internalRepoPath, cloneUrl, ref, skipTLSVerify, proxyOpts, auth, cloneSubmodules, depth)
	if err != nil {
		return nil, err
	}

	if syncResult.State == SyncStateCloned {
		log.Debug("repository cloned", slog.String("path", externalRepoPath))
	}

	return syncResult.Repository, nil
}
