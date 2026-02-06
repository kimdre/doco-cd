package stages

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/utils/set"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

func (s *StageManager) RunPreDeployStage(ctx context.Context, stageLog *slog.Logger) error {
	s.Stages.PreDeploy.StartedAt = time.Now()

	defer func() {
		s.Stages.PreDeploy.FinishedAt = time.Now()
	}()

	latestCommit, err := git.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
	if err != nil {
		return fmt.Errorf("failed to get latest commit: %w", err)
	}

	// Check for external secret changes and current deployed commit
	secretsChanged := false // Flag to indicate if external secrets have changed
	imagesChanged := false  // Flag to indicate if images have changed
	deployedCommit := ""
	curDeployConfigHash := ""
	curSecretHash := ""

	serviceLabels, err := docker.GetServiceLabels(ctx, s.Docker.Client, s.DeployConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to retrieve service labels: %w", err)
	}

	// Find deployed commit, deployConfig hash and externalSecrets hash from labels of deployed services
	for _, labels := range serviceLabels {
		name, ok := labels[docker.DocoCDLabels.Repository.Name]
		if !ok || name != getFullName(s.Repository.CloneURL) {
			break
		}

		deployedCommit = labels[docker.DocoCDLabels.Deployment.CommitSHA]
		curDeployConfigHash = labels[docker.DocoCDLabels.Deployment.ConfigHash]
		curSecretHash = labels[docker.DocoCDLabels.Deployment.ExternalSecretsHash]
	}

	// Compare external secrets if a secret provider is configured
	if s.SecretProvider != nil && *s.SecretProvider != nil && len(s.DeployConfig.ExternalSecrets) > 0 {
		stageLog.Debug("resolving external secrets", slog.Any("external_secrets", s.DeployConfig.ExternalSecrets))

		// Resolve external secrets
		s.DeployState.ResolvedSecrets, err = (*s.SecretProvider).ResolveSecretReferences(ctx, s.DeployConfig.ExternalSecrets)
		if err != nil {
			return fmt.Errorf("failed to resolve external secrets: %w", err)
		}

		secretHash := secretprovider.Hash(s.DeployState.ResolvedSecrets)
		if curSecretHash != "" && curSecretHash != secretHash {
			stageLog.Debug("external secrets have changed, proceeding with deployment")

			secretsChanged = true
		}
	}

	s.DeployConfig.Internal.Hash, err = s.DeployConfig.Hash()
	if err != nil {
		return fmt.Errorf("failed to hash deploy configuration: %w", err)
	}

	if s.DeployConfig.ForceImagePull {
		stageLog.Debug("force image pull enabled, checking for image updates")

		var (
			beforeImages set.Set[string]
			afterImages  set.Set[string]
		)

		containers, _ := docker.GetProjectContainers(ctx, s.Docker.Cmd, s.DeployConfig.Name)

		if len(containers) > 0 {
			beforeImages, err = docker.GetImages(ctx, s.Docker.Cmd, s.DeployConfig.Name)
			if err != nil {
				return fmt.Errorf("failed to get images before pull: %w", err)
			}

			err = docker.PullImages(ctx, s.Docker.Cmd, s.DeployConfig.Name)
			if err != nil {
				return fmt.Errorf("failed to pull images: %w", err)
			}

			afterImages, err = docker.GetImages(ctx, s.Docker.Cmd, s.DeployConfig.Name)
			if err != nil {
				return fmt.Errorf("failed to get images after pull: %w", err)
			}

			for img := range afterImages {
				if !beforeImages.Contains(img) {
					imagesChanged = true
					break
				}
			}

			if imagesChanged {
				stageLog.Debug("images have changed after pull, proceeding with deployment")
			} else {
				stageLog.Debug("images have not changed after pull")
			}
		} else {
			stageLog.Debug("no running containers found for the deployment, skipping image pull check")
		}
	}

	stageLog.Debug("comparing commits",
		slog.String("deployed_commit", deployedCommit),
		slog.String("latest_commit", latestCommit))

	// If no new commit and secret values have not changed, skip deployment
	if latestCommit == deployedCommit && !secretsChanged && !imagesChanged {
		stageLog.Debug("no new commit found, skipping deployment", slog.String("last_commit", latestCommit))

		return ErrSkipDeployment
	}

	if deployedCommit != "" {
		// Check for file changes
		s.DeployState.ChangedFiles, err = git.GetChangedFilesBetweenCommits(s.Repository.Git, plumbing.NewHash(deployedCommit), plumbing.NewHash(latestCommit))
		if err != nil {
			return fmt.Errorf("failed to get changed files between commits: %w", err)
		}

		filesChanged, err := git.HasChangesInSubdir(s.DeployState.ChangedFiles, s.Repository.PathInternal, s.DeployConfig.WorkingDirectory)
		if err != nil {
			return fmt.Errorf("failed to compare commits in subdirectory: %w", err)
		}

		// Skip deployConfig hash check if the label is missing (added in a later version) to avoid unnecessary deployments;
		// it will be updated on the next deployment after upgrade.
		// Compare deployConfig hashes
		deployConfigChanged := curDeployConfigHash != "" && curDeployConfigHash != s.DeployConfig.Internal.Hash
		if deployConfigChanged {
			stageLog.Debug("deploy configuration has changed", slog.String("new_hash", s.DeployConfig.Internal.Hash), slog.String("old_hash", curDeployConfigHash))
		}

		if !deployConfigChanged && !filesChanged && !secretsChanged && !imagesChanged {
			stageLog.Debug("no changes detected, skipping deployment",
				slog.String("directory", s.DeployConfig.WorkingDirectory))

			return ErrSkipDeployment
		}

		stageLog.Debug("changes detected, proceeding with deployment",
			slog.String("directory", s.DeployConfig.WorkingDirectory),
			slog.Group("has_changes",
				slog.Bool("files", filesChanged),
				slog.Bool("deploy_config", deployConfigChanged),
				slog.Bool("external_secrets", secretsChanged),
				slog.Bool("images", imagesChanged),
			),
		)
	}

	return nil
}
