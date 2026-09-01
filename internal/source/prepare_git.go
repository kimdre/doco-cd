package source

import (
	"log/slog"
	"strings"

	"github.com/kimdre/doco-cd/internal/git"
)

// prepareGit resolves req's Git repository into internalRepoPath (cloning or
// updating it as needed) and returns the resolved immutable revision (commit
// SHA). initialRevision seeds the returned revision when neither the fast
// path nor GetLatestCommit resolve one (e.g. an OCI digest carried over from
// the payload, mirrored from the pre-refactor handler for parity - it never
// applies on the Git path in practice).
//
// As an optimization, when the webhook payload carries the exact commit SHA
// and the local repository HEAD already matches it (e.g. webhook
// re-deliveries), the network fetch is skipped entirely.
func (p *Preparer) prepareGit(req Request, internalRepoPath, externalRepoPath, initialRevision string) (string, error) {
	resolvedRevision := initialRevision

	if sha := strings.TrimSpace(req.Payload.CommitSHAString()); sha != "" {
		if matches, _ := git.HeadMatchesCommit(internalRepoPath, sha); matches {
			req.Logger.Debug("skipping fetch, repository already at requested commit", slog.String("commit", sha))

			if repo, openErr := git.OpenRepository(internalRepoPath); openErr == nil {
				if latestCommit, latestErr := git.GetLatestCommit(repo, req.Ref); latestErr == nil {
					resolvedRevision = strings.TrimSpace(latestCommit)
				}
			}

			return resolvedRevision, nil
		}
	}

	repo, err := git.CloneOrUpdateRepository(req.Logger,
		req.SourceRef, req.Ref, internalRepoPath, externalRepoPath,
		req.Private, p.appConfig.SSHPrivateKey, p.appConfig.SSHPrivateKeyPassphrase, p.appConfig.GitAccessToken,
		p.appConfig.SkipTLSVerification, p.appConfig.HttpProxy, p.appConfig.GitCloneSubmodules, p.appConfig.GitCloneDepth,
	)
	if err != nil {
		return resolvedRevision, wrapPrepareError(ErrGitClone, err)
	}

	latestCommit, err := git.GetLatestCommit(repo, req.Ref)
	if err == nil {
		resolvedRevision = strings.TrimSpace(latestCommit)
	}

	return resolvedRevision, nil
}
