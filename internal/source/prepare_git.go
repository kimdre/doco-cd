package source

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/migration"
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

	if err := p.ensureGitStoreMigrated(ctx, req, storeBaseDir); err != nil {
		return result, wrapPrepareError(ErrGitClone, err)
	}

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

	resolveStartedAt := time.Now()

	revision, err := gitStore.Resolve(ctx, req.Ref)
	if err != nil {
		return result, wrapPrepareError(ErrGitClone, err)
	}

	req.Logger.Debug("fetched and resolved git reference",
		slog.String("reference", req.Ref),
		slog.String("revision", string(revision)),
		slog.String("elapsed_time", time.Since(resolveStartedAt).Truncate(time.Millisecond).String()))

	publishStartedAt := time.Now()

	artifact, err := gitStore.Publish(ctx, revision)
	if err != nil {
		return result, wrapPrepareError(ErrGitClone, err)
	}

	req.Logger.Debug("published and decrypted git artifact",
		slog.String("revision", string(revision)),
		slog.String("elapsed_time", time.Since(publishStartedAt).Truncate(time.Millisecond).String()))

	result.revision = string(revision)
	result.artifactPath = artifact.Path

	return result, nil
}

// ensureGitStoreMigrated finishes migrating storeBaseDir from a legacy checkout to the current
// store layout if it discovers one still there, before GitStore ever touches it.
//
// Startup migration normally handles this once, before doco-cd starts accepting webhooks or
// polling. But it defers a repository's mirror bootstrap while a running container still
// references its old, legacy working tree - and, since migration only runs once per process
// start, that deferral otherwise lasts until the next full restart. Without this call, GitStore's
// mirror clone has no awareness of the still-present legacy checkout: finding no mirror yet, it
// would clone a fresh, independent one alongside the un-migrated legacy content instead of
// replacing it, silently duplicating history and merging unrelated artifacts.
//
// Prepare already holds a shared GC lock on storeBaseDir for the duration of this call, which is
// exactly the precondition migration.MigrateRepository requires.
func (p *Preparer) ensureGitStoreMigrated(ctx context.Context, req Request, storeBaseDir string) error {
	if p.contexts == nil {
		// No Docker context registry configured: behave as if the repository were never a
		// legacy checkout. Always correct for a store with no on-disk data yet, and the only
		// reasonable fallback otherwise (see Dependencies.Contexts).
		return nil
	}

	migrated, err := migration.MigrateRepository(ctx, req.Logger, p.contexts,
		req.DataMountPoint.Source, req.DataMountPoint.Destination, storeBaseDir)
	if err != nil {
		return fmt.Errorf("migrate legacy repository layout: %w", err)
	}

	if !migrated {
		return fmt.Errorf("%w: %s", git.ErrLegacyCheckoutNotMigrated, storeBaseDir)
	}

	return nil
}
