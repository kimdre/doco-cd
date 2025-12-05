package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// RunInitStage executes the initialization stage logic for the deployment process.
func (s *StageManager) RunInitStage(ctx context.Context) error {
	s.Stages.Init.StartedAt = time.Now()

	defer func() {
		s.Stages.Init.FinishedAt = time.Now()
	}()

	if s.JobTrigger == JobTriggerWebhook {
		// Skip deployment if the webhook event does not match the filter
		if s.DeployConfig.WebhookEventFilter != "" {
			filter := regexp.MustCompile(s.DeployConfig.WebhookEventFilter)
			if !filter.MatchString(s.Payload.Ref) {
				s.Log.Debug("reference does not match the webhook event filter, skipping deployment",
					slog.String("webhook_filter", s.DeployConfig.WebhookEventFilter), slog.String("ref", s.Payload.Ref))

				return ErrSkipDeployment
			}

			s.Log.Debug("reference matches the webhook event filter, proceeding with deployment",
				slog.String("webhook_filter", s.DeployConfig.WebhookEventFilter), slog.String("ref", s.Payload.Ref))
		}
	}

	err := config.LoadLocalDotEnv(s.DeployConfig, s.Repository.PathInternal)
	if err != nil {
		return fmt.Errorf("failed to parse local env files: %w", err)
	}

	s.Repository.PathInternal, err = filesystem.VerifyAndSanitizePath(filepath.Join(s.Docker.DataMountPoint.Destination, s.Repository.Name), s.Docker.DataMountPoint.Destination) // Path inside the container
	if err != nil {
		return fmt.Errorf("failed to verify and sanitize internal filesystem path: %w", err)
	}

	s.Repository.PathExternal, err = filesystem.VerifyAndSanitizePath(filepath.Join(s.Docker.DataMountPoint.Source, s.Repository.Name), s.Docker.DataMountPoint.Source) // Path on the host
	if err != nil {
		return fmt.Errorf("failed to verify and sanitize external filesystem path: %w", err)
	}

	s.Log = s.Log.With(
		slog.String("stack", s.DeployConfig.Name),
		slog.String("repository", s.Repository.Name),
		slog.String("reference", s.DeployConfig.Reference),
	)

	authCloneUrl := string(s.Repository.CloneURL)
	if s.AppConfig.GitAccessToken != "" {
		authCloneUrl = git.GetAuthUrl(string(s.Repository.CloneURL), s.AppConfig.AuthType, s.AppConfig.GitAccessToken)
	}

	if s.DeployConfig.RepositoryUrl != "" {
		s.Log.Debug("repository URL provided, cloning remote repository")

		s.Repository.CloneURL = s.DeployConfig.RepositoryUrl

		_, err = git.CloneRepository(s.Repository.PathInternal, authCloneUrl, s.DeployConfig.Reference, s.AppConfig.SkipTLSVerification, s.AppConfig.HttpProxy)
		if err != nil && !errors.Is(err, git.ErrRepositoryAlreadyExists) {
			return fmt.Errorf("failed to clone repository: %w", err)
		}

		s.Log.Info("cloned remote repository",
			slog.String("url", string(s.Repository.CloneURL)),
			slog.String("path", s.Repository.PathExternal))
	}

	if s.DeployConfig.Destroy {
		// Skip deployment if another project with the same name already exists
		// Check if containers do not belong to this repository or if doco-cd does not manage the stack
		correctRepo := true

		serviceLabels, err := docker.GetServiceLabels(ctx, s.Docker.Client, s.DeployConfig.Name)
		if err != nil {
			return fmt.Errorf("failed to retrieve service labels: %w", err)
		}

		for _, labels := range serviceLabels {
			name, ok := labels[docker.DocoCDLabels.Repository.Name]

			fmt.Println("name:", name)
			fmt.Println("expected name:", getFullName(s.Repository.CloneURL))

			if !ok || name != getFullName(s.Repository.CloneURL) {
				correctRepo = false
				break
			}
		}

		if !correctRepo {
			return fmt.Errorf("%w: %s: skipping deployment", ErrDeploymentConflict, s.DeployConfig.Name)
		}
	}

	s.Log.Debug("checking out reference "+s.DeployConfig.Reference, slog.String("path", s.Repository.PathExternal))

	s.Repository.Git, err = git.UpdateRepository(s.Repository.PathInternal, authCloneUrl, s.DeployConfig.Reference, s.AppConfig.SkipTLSVerification, s.AppConfig.HttpProxy)
	if err != nil {
		return fmt.Errorf("failed to checkout repository: %w", err)
	}

	if s.JobTrigger == JobTriggerPoll {
		s.Payload = &webhook.ParsedPayload{
			Name:      getRepoName(s.Repository.CloneURL),
			Ref:       s.DeployConfig.Reference,
			CommitSHA: string(JobTriggerPoll),
			FullName:  getFullName(s.Repository.CloneURL),
			CloneURL:  string(s.Repository.CloneURL),
			WebURL:    string(s.Repository.CloneURL),
		}
	}

	fmt.Println(s.Payload)

	return nil
}
