package stages

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"regexp"
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

	// Git sources deliberately reset to the store's base directory here:
	// the store below republishes this stack's own reference out of it.
	// For OCI sources Prepare already resolved these paths to the published artifact directory,
	// and recomputing them would point the deployment at the base directory instead -
	// which holds the pull cache and every published artifact, but no compose file.
	if s.Repository.Source != config.SourceTypeOCI || s.DeployConfig.RepositoryUrl != "" {
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
	s.releaseGCLock, err = sourcecache.AcquireSharedGCPathLock(s.Repository.PathInternal)
	if err != nil {
		return fmt.Errorf("acquire artifact GC lock: %w", err)
	}

	// Ask the immutable per-revision store for this stack's reference,
	// rather than checking out a path shared with every other stack/
	// reference using the same repository (whether that's the primary
	// source repository, or one named by this stack's own RepositoryUrl).
	// GitStore locks its own mirror internally, so concurrent stacks
	// resolving different references (or the same one) no longer need a
	// wrapping lock here to stay correct.
	gitStore, err := store.NewGitStore(store.GitStoreOptions{
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
	if err != nil {
		return fmt.Errorf("failed to initialize git store: %w", err)
	}

	revision, err := gitStore.Resolve(ctx, s.DeployConfig.Reference)
	if err != nil {
		return fmt.Errorf("failed to resolve reference %s: %w", s.DeployConfig.Reference, err)
	}

	artifact, err := gitStore.Publish(ctx, revision)
	if err != nil {
		return fmt.Errorf("failed to publish artifact for revision %s: %w", revision, err)
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

	mirrorRepo, err := git.OpenRepository(gitStore.MirrorDir())
	if err != nil {
		return fmt.Errorf("failed to open repository mirror: %w", err)
	}

	s.Repository.Git = mirrorRepo
	s.Repository.MirrorDir = gitStore.MirrorDir()

	stageLog.Debug("resolved repository artifact",
		slog.String("url", s.Repository.SourceUrl),
		slog.String("reference", s.DeployConfig.Reference),
		slog.String("revision", string(revision)),
		slog.String("path", s.Repository.PathExternal))

	if s.DeployConfig.RepositoryUrl != "" {
		// Now also load remote dotenv files.
		err = deploy.LoadLocalDotEnv(s.DeployConfig, filepath.Join(s.Repository.PathInternal, s.DeployConfig.WorkingDirectory))
		if err != nil {
			return fmt.Errorf("failed to parse remote env files: %w", err)
		}

		// Now also load remote external secrets files.
		err = deploy.LoadExternalSecretsFiles(s.DeployConfig, filepath.Join(s.Repository.PathInternal, s.DeployConfig.WorkingDirectory))
		if err != nil {
			return fmt.Errorf("failed to parse remote external secrets files: %w", err)
		}
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

// MatchesWebhookEventFilter reports whether this run should proceed based on
// its trigger, configured webhook filter, and payload reference.
func (s *StageManager) MatchesWebhookEventFilter() bool {
	if s.JobTrigger != JobTriggerWebhook || s.DeployConfig.WebhookEventFilter == "" {
		return true
	}

	return s.Payload != nil && regexp.MustCompile(s.DeployConfig.WebhookEventFilter).MatchString(s.Payload.Ref)
}
