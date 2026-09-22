package stages

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/source/oci"
	"github.com/kimdre/doco-cd/internal/source/store"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func mergeDeploymentEnvironment(config *deploy.Config) {
	if len(config.Environment) == 0 {
		return
	}

	if config.Internal.Environment == nil {
		config.Internal.Environment = make(map[string]string)
	}

	maps.Copy(config.Internal.Environment, config.Environment)
}

func useDeploymentGitRepository(repository *RepositoryData, repositoryURL config.GitUrl) {
	repository.Source = config.SourceTypeGit
	repository.SourceUrl = string(repositoryURL)
	repository.Name = git.GetRepoName(repository.SourceUrl)
}

func loadConfigSourceFiles(deployConfig *deploy.Config, sourcePath string) error {
	if deployConfig.Internal.ConfigSourceFilesLoaded {
		return nil
	}

	if err := deploy.LoadLocalDotEnv(deployConfig, sourcePath); err != nil {
		return fmt.Errorf("failed to parse local env files: %w", err)
	}

	if err := deploy.LoadExternalSecretsFiles(deployConfig, sourcePath); err != nil {
		return fmt.Errorf("failed to parse local external secrets files: %w", err)
	}

	deployConfig.Internal.ConfigSourceFilesLoaded = true

	return nil
}

// RunInitStage executes the initialization stage logic for the deployment process.
func (s *StageManager) RunInitStage(ctx context.Context, stageLog *slog.Logger) error {
	var err error

	s.Stages.Init.StartedAt = time.Now()

	defer func() {
		s.Stages.Init.FinishedAt = time.Now()
	}()

	if s.JobTrigger == JobTriggerWebhook {
		// Skip deployment if the webhook event does not match the filter
		if s.DeployConfig.WebhookEventFilter != "" {
			if !s.MatchesWebhookEventFilter() {
				stageLog.Debug("reference does not match the webhook event filter, skipping deployment",
					slog.String("webhook_filter", s.DeployConfig.WebhookEventFilter), slog.String("ref", s.Payload.Ref))

				return ErrWebhookFilterMismatch
			}

			stageLog.Debug("reference matches the webhook event filter, proceeding with deployment",
				slog.String("webhook_filter", s.DeployConfig.WebhookEventFilter), slog.String("ref", s.Payload.Ref))
		}
	}

	if s.DeployConfig.RepositoryUrl != "" {
		useDeploymentGitRepository(s.Repository, s.DeployConfig.RepositoryUrl)

		// Reconciliation reuses files loaded from the immutable config artifact during the initial deployment. This keeps
		// later runs independent of that artifact after GC removes it.
		if err = loadConfigSourceFiles(s.DeployConfig, s.Repository.PathInternal); err != nil {
			return err
		}
	}

	// A stack tracking the same repository/reference that Prepare already resolved for this job
	// (the common case for auto-discovered stacks on the triggering branch) can reuse that
	// resolution instead of paying for its own redundant fetch: Prepare already ran Resolve+Publish
	// once for the whole job, and RunInitStage would otherwise re-run gitStore.Resolve (a real
	// network fetch guarded by the mirror's exclusive path lock) for every single matched stack,
	// serializing them all behind that one shared mirror.
	// A stack that overrides git_depth needs its own Resolve: Prepare mirrors at the global
	// depth, and reusing that shallower mirror would hide the history the stack asked for from
	// the deployed-commit lookup and the changed-file/changelog comparisons in the later stages.
	fastPathEligible := s.DeployConfig.RepositoryUrl == "" &&
		s.Repository.Source != config.SourceTypeOCI &&
		s.Repository.MirrorDir != "" &&
		s.Repository.Revision != "" &&
		s.Repository.PathInternal != "" &&
		resolvedReferenceMatches(s.Repository.ResolvedReference, s.DeployConfig.Reference) &&
		s.DeployConfig.ResolveGitDepth(s.AppConfig.GitCloneDepth) == s.AppConfig.GitCloneDepth

	// Git sources deliberately reset to the store's base directory here:
	// the store below republishes this stack's own reference out of it.
	// For OCI sources Prepare already resolved these paths to the published artifact directory,
	// and recomputing them would point the deployment at the base directory instead -
	// which holds the pull cache and every published artifact, but no compose file.
	// The fast path above keeps Prepare's already-published artifact paths instead: there is
	// nothing left to republish, so resetting to the base directory here would just be undone.
	if (s.Repository.Source != config.SourceTypeOCI && !fastPathEligible) || s.DeployConfig.RepositoryUrl != "" {
		s.Repository.PathInternal, err = filesystem.VerifyAndSanitizePath(filepath.Join(s.Docker.DataMountPoint.Destination, s.Repository.Name), s.Docker.DataMountPoint.Destination) // Path inside the container
		if err != nil {
			return fmt.Errorf("failed to verify and sanitize internal filesystem path: %w", err)
		}

		s.Repository.PathExternal, err = filesystem.VerifyAndSanitizePath(filepath.Join(s.Docker.DataMountPoint.Source, s.Repository.Name), s.Docker.DataMountPoint.Source) // Path on the host
		if err != nil {
			return fmt.Errorf("failed to verify and sanitize external filesystem path: %w", err)
		}
	}

	if s.Repository.Source == config.SourceTypeOCI {
		artifactsDir := filepath.Dir(s.Repository.PathInternal)
		if filepath.Base(artifactsDir) != store.ArtifactsSubdir {
			return fmt.Errorf("invalid OCI artifact path: %s", s.Repository.PathInternal)
		}

		storeBaseDir := filepath.Dir(artifactsDir)

		s.releaseGCLock, err = sourcecache.AcquireSharedGCPathLock(storeBaseDir)
		if err != nil {
			return fmt.Errorf("acquire artifact GC lock: %w", err)
		}

		override := oci.SelectTrustPolicyOverride(s.DeployConfig.Oci, s.DeployConfig.Internal.OciTrustPolicyOverrideTrusted)

		// OCI artifacts are verified before config parsing and reconciliation cleanup.
		// Re-verify here only when this deployment config provides a trusted override
		// or when the repository has not been pre-verified.
		if !s.Repository.OCITrusted || s.DeployConfig.Internal.OciTrustPolicyOverrideTrusted {
			if err := oci.VerifyWithCosign(ctx, s.Repository.SourceUrl, s.Repository.Revision, s.AppConfig.OciTrustPolicy, override, s.AppConfig.OciVerifyMaxWorkers); err != nil {
				return fmt.Errorf("failed OCI signature verification: %w", err)
			}
		}

		ociStore, err := store.NewOCIStore(store.OCIStoreOptions{
			Log:                 stageLog,
			ArtifactRef:         s.Repository.SourceUrl,
			BaseDir:             storeBaseDir,
			CustomTarget:        s.DeployConfig.Internal.ConfigTarget,
			TrustPolicy:         s.AppConfig.OciTrustPolicy,
			TrustPolicyOverride: override,
			VerifyMaxWorkers:    s.AppConfig.OciVerifyMaxWorkers,
		})
		if err != nil {
			return fmt.Errorf("failed to initialize OCI store: %w", err)
		}

		artifact, err := ociStore.Publish(ctx, store.Revision(s.Repository.Revision))
		if err != nil {
			return fmt.Errorf("failed to publish OCI artifact %s: %w", s.Repository.Revision, err)
		}

		s.Repository.PathInternal = artifact.Path

		err = deploy.LoadLocalDotEnv(s.DeployConfig, filepath.Join(s.Repository.PathInternal, s.DeployConfig.WorkingDirectory))
		if err != nil {
			return fmt.Errorf("failed to parse env files from OCI artifact: %w", err)
		}

		err = deploy.LoadExternalSecretsFiles(s.DeployConfig, filepath.Join(s.Repository.PathInternal, s.DeployConfig.WorkingDirectory))
		if err != nil {
			return fmt.Errorf("failed to parse external secrets files from OCI artifact: %w", err)
		}

		mergeDeploymentEnvironment(s.DeployConfig)
		deploy.MergeExternalSecretsFromFiles(s.DeployConfig)

		s.Log = s.Log.With(
			slog.String("stack", s.DeployConfig.Name),
			slog.String("repository", s.Repository.Name),
		)

		return nil
	}

	stageLog = stageLog.With(
		slog.String("stack", s.DeployConfig.Name),
		slog.String("repository", s.Repository.Name),
		slog.String("reference", s.DeployConfig.Reference),
	)

	// This deployment may resolve a different repository or revision than
	// Prepare did. Hold that store's GC gate until RunStages finishes.
	//
	// The GC gate is always keyed by the store's base directory (not the resolved artifact directory),
	// matching every other acquisition of this lock (Prepare, OCI branch above, GC sweep itself).
	// In the fast path below, s.Repository.PathInternal already points at Prepare's
	// resolved artifact directory rather than the base directory - it must be recomputed here
	// rather than reused, or this would take a lock on the wrong path and not actually exclude GC.
	gcBaseDir, err := filesystem.VerifyAndSanitizePath(filepath.Join(s.Docker.DataMountPoint.Destination, s.Repository.Name), s.Docker.DataMountPoint.Destination)
	if err != nil {
		return fmt.Errorf("failed to verify and sanitize internal filesystem path: %w", err)
	}

	s.releaseGCLock, err = sourcecache.AcquireSharedGCPathLock(gcBaseDir)
	if err != nil {
		return fmt.Errorf("acquire artifact GC lock: %w", err)
	}

	if fastPathEligible {
		// Prepare already resolved and published this exact repository/reference for this job -
		// reuse that artifact/revision/mirror as-is instead of re-running Resolve+Publish (a real
		// network fetch) for this stack too. The mirror is only probed here to fail fast if it is
		// unreadable; the handle is deliberately discarded, because later stages must open their
		// own short-lived handle per read (see StageManager.withMirrorRead).
		if err := verifyMirrorReadable(s.Repository.MirrorDir); err != nil {
			return err
		}

		stageLog.Debug("reusing already-resolved repository artifact",
			slog.String("url", s.Repository.SourceUrl),
			slog.String("reference", s.DeployConfig.Reference),
			slog.String("revision", s.Repository.Revision),
			slog.String("path", s.Repository.PathExternal))
	} else {
		// Ask the immutable per-revision store for this stack's reference,
		// rather than checking out a path shared with every other stack/
		// reference using the same repository (whether that's the primary
		// source repository, or one named by this stack's own RepositoryUrl).
		// GitStore locks its own mirror internally, so concurrent stacks
		// resolving different references (or the same one) no longer need a
		// wrapping lock here to stay correct.
		gitStore, gitStoreErr := store.NewGitStore(store.GitStoreOptions{
			Log:                     stageLog,
			CloneURL:                s.Repository.SourceUrl,
			BaseDir:                 s.Repository.PathInternal,
			SSHPrivateKey:           s.AppConfig.SSHPrivateKey,
			SSHPrivateKeyPassphrase: s.AppConfig.SSHPrivateKeyPassphrase,
			AccessToken:             s.AppConfig.GitAccessToken,
			SkipTLSVerify:           s.AppConfig.SkipTLSVerification,
			ProxyOptions:            s.AppConfig.HttpProxy,
			CloneSubmodules:         s.AppConfig.GitCloneSubmodules,
			Depth:                   s.DeployConfig.ResolveGitDepth(s.AppConfig.GitCloneDepth),
		})
		if gitStoreErr != nil {
			return fmt.Errorf("failed to initialize git store: %w", gitStoreErr)
		}

		revision, resolveErr := gitStore.Resolve(ctx, s.DeployConfig.Reference)
		if resolveErr != nil {
			return fmt.Errorf("failed to resolve reference %s: %w", s.DeployConfig.Reference, resolveErr)
		}

		artifact, pErr := gitStore.Publish(ctx, revision)
		if pErr != nil {
			return fmt.Errorf("failed to publish artifact for revision %s: %w", revision, pErr)
		}

		rel, relErr := filepath.Rel(s.Docker.DataMountPoint.Destination, artifact.Path)
		if relErr != nil {
			return fmt.Errorf("failed to compute external path for artifact: %w", relErr)
		}

		s.Repository.PathInternal = artifact.Path
		s.Repository.PathExternal = filepath.Join(s.Docker.DataMountPoint.Source, rel)

		// This stack deploys exactly this revision - its deploy config may name a
		// different reference (or a different repository) than the event that
		// triggered the run resolved to. Recording it means later stages compare
		// against and label with what was published here, instead of re-resolving
		// the reference against a mirror another run may have advanced in the meantime.
		s.Repository.Revision = string(revision)

		s.Repository.MirrorDir = gitStore.MirrorDir()

		if err := verifyMirrorReadable(s.Repository.MirrorDir); err != nil {
			return err
		}

		stageLog.Debug("resolved repository artifact",
			slog.String("url", s.Repository.SourceUrl),
			slog.String("reference", s.DeployConfig.Reference),
			slog.String("revision", string(revision)),
			slog.String("path", s.Repository.PathExternal))
	}

	// Load dotenv and external secrets files relative to the working directory. This must run
	// unconditionally (not just when RepositoryUrl is set), otherwise files placed anywhere other
	// than the repo root are never loaded for the common case of a deploy config in the same
	// repository that triggered the deployment.
	envFileKind := "local"
	if s.DeployConfig.RepositoryUrl != "" {
		envFileKind = "remote"
	}

	// Load local dotenv files.
	err = deploy.LoadLocalDotEnv(s.DeployConfig, filepath.Join(s.Repository.PathInternal, s.DeployConfig.WorkingDirectory))
	if err != nil {
		return fmt.Errorf("failed to parse %s env files: %w", envFileKind, err)
	}

	// Load external secrets files.
	err = deploy.LoadExternalSecretsFiles(s.DeployConfig, filepath.Join(s.Repository.PathInternal, s.DeployConfig.WorkingDirectory))
	if err != nil {
		return fmt.Errorf("failed to parse %s external secrets files: %w", envFileKind, err)
	}

	mergeDeploymentEnvironment(s.DeployConfig)
	deploy.MergeExternalSecretsFromFiles(s.DeployConfig)

	if s.DeployConfig.Destroy.Enabled {
		// Skip deployment if another project with the same name already exists
		// Check if containers do not belong to this repository or if doco-cd does not manage the stack
		correctRepo := true

		serviceLabels, err := docker.GetServiceLabels(ctx, s.Docker.Cmd.Client(), s.Docker.SwarmMode, s.DeployConfig.Name)
		if err != nil {
			return fmt.Errorf("failed to retrieve service labels: %w", err)
		}

		for _, labels := range serviceLabels {
			name, ok := labels[docker.DocoCDLabels.Source.Name]

			if !ok || name != git.GetFullName(s.Repository.SourceUrl) {
				correctRepo = false
				break
			}
		}

		if !correctRepo {
			return fmt.Errorf("%w: %s: skipping deployment", ErrDeploymentConflict, s.DeployConfig.Name)
		}
	}

	if s.JobTrigger == JobTriggerPoll {
		if s.Repository.Source == config.SourceTypeOCI {
			s.Payload = &webhook.ParsedPayload{
				Source:    webhook.PayloadSourceOCI,
				Name:      s.Repository.Name,
				Ref:       s.DeployConfig.Reference,
				CommitSHA: plumbing.ZeroHash,
				Trigger:   s.Repository.Revision,
				FullName:  s.Repository.Name,
				WebURL:    s.Repository.SourceUrl,
				Artifact:  s.Repository.SourceUrl,
				Digest:    s.Repository.Revision,
			}
		} else {
			s.Payload = &webhook.ParsedPayload{
				Source:    webhook.PayloadSourceGit,
				Name:      git.GetRepoName(s.Repository.SourceUrl),
				Ref:       s.DeployConfig.Reference,
				CommitSHA: plumbing.ZeroHash,
				Trigger:   string(JobTriggerPoll),
				FullName:  git.GetFullName(s.Repository.SourceUrl),
				CloneURL:  s.Repository.SourceUrl,
				WebURL:    s.Repository.SourceUrl,
			}
		}
	}

	if s.Repository.Source == config.SourceTypeOCI {
		s.Log = s.Log.With(
			slog.String("stack", s.DeployConfig.Name),
			slog.String("repository", s.Repository.Name),
		)
	} else {
		s.Log = s.Log.With(
			slog.String("stack", s.DeployConfig.Name),
			slog.String("repository", s.Repository.Name),
			slog.String("reference", s.DeployConfig.Reference),
		)
	}

	return nil
}

// resolvedReferenceMatches treats a webhook's fully qualified branch reference
// as equivalent to the short branch name accepted by deployment configs.
// Keep the normalization asymmetric: a short configured tag can be ambiguous
// with a same-named branch, while refs/heads/<name> unambiguously identifies
// the branch Prepare resolved.
func resolvedReferenceMatches(resolvedReference, configuredReference string) bool {
	if resolvedReference == configuredReference {
		return true
	}

	branch, ok := strings.CutPrefix(resolvedReference, git.BranchPrefix)

	return ok && branch == configuredReference
}

// MatchesWebhookEventFilter reports whether this run should proceed based on
// its trigger, configured webhook filter, and payload reference.
func (s *StageManager) MatchesWebhookEventFilter() bool {
	if s.JobTrigger != JobTriggerWebhook || s.DeployConfig.WebhookEventFilter == "" {
		return true
	}

	return s.Payload != nil && regexp.MustCompile(s.DeployConfig.WebhookEventFilter).MatchString(s.Payload.Ref)
}
