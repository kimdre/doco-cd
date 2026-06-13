package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/source/oci"
	"github.com/kimdre/doco-cd/internal/webhook"
)

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
			filter := regexp.MustCompile(s.DeployConfig.WebhookEventFilter)
			if !filter.MatchString(s.Payload.Ref) {
				stageLog.Debug("reference does not match the webhook event filter, skipping deployment",
					slog.String("webhook_filter", s.DeployConfig.WebhookEventFilter), slog.String("ref", s.Payload.Ref))

				return ErrSkipDeployment
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

		if err := oci.VerifyWithCosign(ctx, s.Repository.SourceUrl, s.Repository.Revision, s.AppConfig.OciTrustPolicy, override, s.AppConfig.OciVerifyMaxWorkers); err != nil {
			return fmt.Errorf("failed OCI signature verification: %w", err)
		}

		err = deploy.LoadLocalDotEnv(s.DeployConfig, filepath.Join(s.Repository.PathInternal, s.DeployConfig.WorkingDirectory))
		if err != nil {
			return fmt.Errorf("failed to parse env files from OCI artifact: %w", err)
		}

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

	// Attempt to fetch the remote repository before checking if we can skip cloning/updating,
	// to ensure we have the latest commits and references available locally
	if s.DeployConfig.RepositoryUrl != "" {
		repo, err := git.OpenRepository(s.Repository.PathInternal)
		switch {
		case err == nil:
			err = git.FetchRepository(repo, s.Repository.SourceUrl, s.AppConfig.SkipTLSVerification, s.AppConfig.HttpProxy, auth, s.DeployConfig.ResolveGitDepth(s.AppConfig.GitCloneDepth))
			if err != nil {
				// If fetch failed with corruption indicators, attempt repair
				if git.IsCorruptionError(err) {
					stageLog.Warn("detected corruption during fetch, attempting repository repair",
						slog.String("path", s.Repository.PathExternal))

					if _, repairErr := git.RepairRepository(s.Repository.PathInternal, s.Repository.SourceUrl, s.DeployConfig.Reference,
						s.AppConfig.SkipTLSVerification, s.AppConfig.HttpProxy, auth, s.AppConfig.GitCloneSubmodules,
						s.DeployConfig.ResolveGitDepth(s.AppConfig.GitCloneDepth), stageLog); repairErr != nil {
						return fmt.Errorf("failed to fetch repository and repair attempt failed: %w (repair error: %v)", err, repairErr)
					}
				} else {
					return fmt.Errorf("failed to fetch repository: %w", err)
				}
			}
		case errors.Is(err, git.ErrRepositoryNotExists): // Continue without fetching the repository, it will be cloned later
		default:
			return fmt.Errorf("failed to open repository: %w", err)
		}
	}

	// Check if we can skip cloning/updating because the previous run (initial or a prior deploy config)
	skipCloneUpdate, err := git.MatchesHead(s.Repository.PathInternal, s.DeployConfig.Reference)
	if err != nil {
		return fmt.Errorf("failed to check if repository matches remote and reference: %w", err)
	}

	if s.DeployConfig.RepositoryUrl != "" {
		if skipCloneUpdate {
			stageLog.Debug("skipping clone of remote repository, already at correct state",
				slog.String("url", s.Repository.SourceUrl),
				slog.String("reference", s.DeployConfig.Reference))
		} else {
			stageLog.Debug("repository URL provided, cloning remote repository")

			_, err = git.CloneRepository(s.Repository.PathInternal, s.Repository.SourceUrl, s.DeployConfig.Reference,
				s.AppConfig.SkipTLSVerification, s.AppConfig.HttpProxy, auth, s.AppConfig.GitCloneSubmodules, s.DeployConfig.ResolveGitDepth(s.AppConfig.GitCloneDepth))
			if err != nil && !errors.Is(err, git.ErrRepositoryAlreadyExists) {
				return fmt.Errorf("failed to clone repository: %w", err)
			}

			stageLog.Info("cloned remote repository",
				slog.String("url", s.Repository.SourceUrl),
				slog.String("path", s.Repository.PathExternal))
		}

		// Now also load remote dotenv files
		err = deploy.LoadLocalDotEnv(s.DeployConfig, filepath.Join(s.Repository.PathInternal, s.DeployConfig.WorkingDirectory))
		if err != nil {
			return fmt.Errorf("failed to parse remote env files: %w", err)
		}
	}

	if len(s.DeployConfig.Environment) > 0 {
		if s.DeployConfig.Internal.Environment == nil {
			s.DeployConfig.Internal.Environment = make(map[string]string)
		}

		maps.Copy(s.DeployConfig.Internal.Environment, s.DeployConfig.Environment)
	}

	if s.DeployConfig.Destroy.Enabled {
		// Skip deployment if another project with the same name already exists
		// Check if containers do not belong to this repository or if doco-cd does not manage the stack
		correctRepo := true

		serviceLabels, err := docker.GetServiceLabels(ctx, s.Docker.Cmd.Client(), s.DeployConfig.Name)
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

	// Skip UpdateRepository if the previous run already cloned/updated with the same URL and reference
	if skipCloneUpdate {
		stageLog.Debug("skipping checkout, already at correct reference",
			slog.String("reference", s.DeployConfig.Reference),
			slog.String("path", s.Repository.PathExternal))

		s.Repository.Git, err = git.OpenRepository(s.Repository.PathInternal)
		if err != nil {
			return fmt.Errorf("failed to open repository: %w", err)
		}
	} else {
		stageLog.Debug("checking out reference "+s.DeployConfig.Reference, slog.String("path", s.Repository.PathExternal))

		s.Repository.Git, err = git.UpdateRepository(s.Repository.PathInternal, s.Repository.SourceUrl, s.DeployConfig.Reference,
			s.AppConfig.SkipTLSVerification, s.AppConfig.HttpProxy, auth, s.AppConfig.GitCloneSubmodules, s.DeployConfig.ResolveGitDepth(s.AppConfig.GitCloneDepth))
		if err != nil {
			return fmt.Errorf("failed to checkout repository: %w", err)
		}
	}

	if s.JobTrigger == JobTriggerPoll {
		if s.Repository.Source == config.SourceTypeOCI {
			s.Payload = &webhook.ParsedPayload{
				Source:    webhook.PayloadSourceOCI,
				Name:      s.Repository.Name,
				Ref:       s.DeployConfig.Reference,
				CommitSHA: s.Repository.Revision,
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
				CommitSHA: string(JobTriggerPoll),
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
