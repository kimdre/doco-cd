package stages

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/source/oci"
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
		s.Repository.SourceUrl = string(s.DeployConfig.RepositoryUrl)
		s.Repository.Name = git.GetRepoName(s.Repository.SourceUrl)

		// Load local (without remote: prefix) dotenv files before paths get updated to remote repository
		// Remote dotenv files get read later
		err = deploy.LoadLocalDotEnv(s.DeployConfig, s.Repository.PathInternal)
		if err != nil {
			return fmt.Errorf("failed to parse local env files: %w", err)
		}
	}

	s.Repository.PathInternal, err = filesystem.VerifyAndSanitizePath(filepath.Join(s.Docker.DataMountPoint.Destination, s.Repository.Name), s.Docker.DataMountPoint.Destination) // Path inside the container
	if err != nil {
		return fmt.Errorf("failed to verify and sanitize internal filesystem path: %w", err)
	}

	s.Repository.PathExternal, err = filesystem.VerifyAndSanitizePath(filepath.Join(s.Docker.DataMountPoint.Source, s.Repository.Name), s.Docker.DataMountPoint.Source) // Path on the host
	if err != nil {
		return fmt.Errorf("failed to verify and sanitize external filesystem path: %w", err)
	}

	if s.Repository.Source == config.SourceTypeOCI {
		if _, err := os.Stat(s.Repository.PathInternal); err != nil {
			return fmt.Errorf("failed to access extracted OCI artifact directory: %w", err)
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

		err = deploy.LoadLocalDotEnv(s.DeployConfig, filepath.Join(s.Repository.PathInternal, s.DeployConfig.WorkingDirectory))
		if err != nil {
			return fmt.Errorf("failed to parse env files from OCI artifact: %w", err)
		}

		mergeDeploymentEnvironment(s.DeployConfig)

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

	auth, err := git.GetAuthMethod(s.Repository.SourceUrl, s.AppConfig.SSHPrivateKey, s.AppConfig.SSHPrivateKeyPassphrase, s.AppConfig.GitAccessToken)
	if err != nil {
		return fmt.Errorf("failed to get auth method: %w", err)
	}

	var syncResult *git.SyncResult

	if s.DeployConfig.RepositoryUrl == "" {
		matches, matchErr := git.MatchesHead(s.Repository.PathInternal, s.DeployConfig.Reference)
		if matchErr != nil {
			return fmt.Errorf("failed to check prepared repository state: %w", matchErr)
		}

		if matches && !git.RepositoryNeedsReclone(
			s.Repository.PathInternal,
			s.Repository.SourceUrl,
			s.DeployConfig.ResolveGitDepth(s.AppConfig.GitCloneDepth),
		) {
			repo, openErr := git.OpenRepository(s.Repository.PathInternal)
			if openErr != nil {
				return fmt.Errorf("failed to open prepared repository: %w", openErr)
			}

			syncResult = &git.SyncResult{Repository: repo, State: git.SyncStateCurrent}
		}
	}

	if syncResult == nil {
		var syncErr error

		syncResult, syncErr = git.SyncRepository(
			s.Repository.PathInternal, s.Repository.SourceUrl, s.DeployConfig.Reference,
			s.AppConfig.SkipTLSVerification, s.AppConfig.HttpProxy, auth, s.AppConfig.GitCloneSubmodules,
			s.DeployConfig.ResolveGitDepth(s.AppConfig.GitCloneDepth),
		)
		if syncErr != nil {
			return fmt.Errorf("failed to synchronize repository: %w", syncErr)
		}
	}

	s.Repository.Git = syncResult.Repository
	switch syncResult.State {
	case git.SyncStateCurrent:
		stageLog.Debug("skipping clone of remote repository, already at correct state",
			slog.String("url", s.Repository.SourceUrl),
			slog.String("reference", s.DeployConfig.Reference))
	case git.SyncStateCloned:
		stageLog.Info("cloned remote repository",
			slog.String("url", s.Repository.SourceUrl),
			slog.String("path", s.Repository.PathExternal))
	default:
		stageLog.Debug("updated remote repository",
			slog.String("url", s.Repository.SourceUrl),
			slog.String("reference", s.DeployConfig.Reference),
			slog.String("path", s.Repository.PathExternal))
	}

	if s.DeployConfig.RepositoryUrl != "" {
		// Now also load remote dotenv files.
		err = deploy.LoadLocalDotEnv(s.DeployConfig, filepath.Join(s.Repository.PathInternal, s.DeployConfig.WorkingDirectory))
		if err != nil {
			return fmt.Errorf("failed to parse remote env files: %w", err)
		}
	}

	mergeDeploymentEnvironment(s.DeployConfig)

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
