package source

import (
	"context"
	"log/slog"
	"strings"

	"github.com/kimdre/doco-cd/internal/source/store"
)

// gitPrepareResult is prepareGit's output: the resolved immutable revision (commit SHA),
// the artifact directory holding that revision's decrypted, deployable content,
// and the store's mirror directory (a bare Git clone).
// Used only for read-only reference resolution against references other than the one just resolved, e.g. by auto-discovery.
type gitPrepareResult struct {
	revision     string
	artifactPath string
	mirrorDir    string
}

// prepareGit resolves req's repository and publishes its immutable revision, reusing an existing commit when possible.
func (p *Preparer) prepareGit(ctx context.Context, req Request, storeBaseDir, initialRevision string) (gitPrepareResult, error) {
	result := gitPrepareResult{revision: initialRevision}

	gitStore, err := store.NewGitStore(store.GitStoreOptions{
		Log:                     req.Logger,
		CloneURL:                req.SourceRef,
		BaseDir:                 storeBaseDir,
		Private:                 req.Private,
		SSHPrivateKey:           p.appConfig.SSHPrivateKey,
		SSHPrivateKeyPassphrase: p.appConfig.SSHPrivateKeyPassphrase,
		AccessToken:             p.appConfig.GitAccessToken,
		SkipTLSVerify:           p.appConfig.SkipTLSVerification,
		ProxyOptions:            p.appConfig.HttpProxy,
		CloneSubmodules:         p.appConfig.GitCloneSubmodules,
		Depth:                   p.appConfig.GitCloneDepth,
	})
	if err != nil {
		return result, wrapPrepareError(ErrGitClone, err)
	}

	result.mirrorDir = gitStore.MirrorDir()

	if sha := strings.TrimSpace(req.Payload.CommitSHAString()); sha != "" {
		if artifact, ok, lookupErr := gitStore.Lookup(store.Revision(sha)); lookupErr == nil && ok {
			req.Logger.Debug("skipping fetch, artifact already published for requested commit", slog.String("commit", sha))

			result.revision = sha
			result.artifactPath = artifact.Path

			return result, nil
		}
	}

	revision, err := gitStore.Resolve(ctx, req.Ref)
	if err != nil {
		return result, wrapPrepareError(ErrGitClone, err)
	}

	artifact, err := gitStore.Publish(ctx, revision)
	if err != nil {
		return result, wrapPrepareError(ErrGitClone, err)
	}

	result.revision = string(revision)
	result.artifactPath = artifact.Path

	return result, nil
}
