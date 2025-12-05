package stages

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

func (s *StageManager) RunPreDeployStage(ctx context.Context) error {
	s.Stages.PreDeploy.StartedAt = time.Now()

	defer func() {
		s.Stages.PreDeploy.FinishedAt = time.Now()
	}()

	if s.Repository.Git == nil {
		fmt.Println("repository is nil after checkout")
	} else {
		fmt.Println("repository is not nil after checkout")
	}

	if s.DeployConfig == nil {
		fmt.Println("deploy config is nil in pre-deploy stage")
	} else {
		fmt.Println("deploy config is not nil in pre-deploy stage")
	}

	latestCommit, err := git.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
	if err != nil {
		return fmt.Errorf("failed to get latest commit: %w", err)
	}

	// Check for external secret changes and current deployed commit
	secretsChanged := false // Flag to indicate if external secrets have changed
	deployedCommit := ""
	deployedSecretHash := ""

	serviceLabels, err := docker.GetServiceLabels(ctx, s.Docker.Client, s.DeployConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to retrieve service labels: %w", err)
	}

	// Find deployed commit and external secrets hash from labels of deployed services
	for _, labels := range serviceLabels {
		name, ok := labels[docker.DocoCDLabels.Repository.Name]
		if !ok || name != getFullName(s.Repository.CloneURL) {
			break
		}

		deployedCommit = labels[docker.DocoCDLabels.Deployment.CommitSHA]
		deployedSecretHash = labels[docker.DocoCDLabels.Deployment.ExternalSecretsHash]
	}

	if s.SecretProvider != nil && *s.SecretProvider != nil && len(s.DeployConfig.ExternalSecrets) > 0 {
		s.Log.Debug("resolving external secrets", slog.Any("external_secrets", s.DeployConfig.ExternalSecrets))

		// Resolve external secrets
		s.DeployState.ResolvedSecrets, err = (*s.SecretProvider).ResolveSecretReferences(ctx, s.DeployConfig.ExternalSecrets)
		if err != nil {
			return fmt.Errorf("failed to resolve external secrets: %w", err)
		}

		secretHash := secretprovider.Hash(s.DeployState.ResolvedSecrets)
		if deployedSecretHash != "" && deployedSecretHash != secretHash {
			s.Log.Debug("external secrets have changed, proceeding with deployment")

			secretsChanged = true
		}
	}

	s.Log.Debug("comparing commits",
		slog.String("deployed_commit", deployedCommit),
		slog.String("latest_commit", latestCommit))

	// If no new commit and secret values have not changed, skip deployment
	if latestCommit == deployedCommit && !secretsChanged && !s.DeployConfig.ForceImagePull {
		s.Log.Debug("no new commit found, skipping deployment", slog.String("last_commit", latestCommit))

		return ErrSkipDeployment
	}

	// Check for file changes
	if deployedCommit != "" {
		s.DeployState.ChangedFiles, err = git.GetChangedFilesBetweenCommits(s.Repository.Git, plumbing.NewHash(deployedCommit), plumbing.NewHash(latestCommit))
		if err != nil {
			return fmt.Errorf("failed to get changed files between commits: %w", err)
		}

		filesChanged, err := git.HasChangesInSubdir(s.DeployState.ChangedFiles, s.Repository.PathInternal, s.DeployConfig.WorkingDirectory)
		if err != nil {
			return fmt.Errorf("failed to compare commits in subdirectory: %w", err)
		}

		if !filesChanged && !secretsChanged && !s.DeployConfig.ForceImagePull {
			s.Log.Debug("no changes detected in subdirectory, skipping deployment",
				slog.String("directory", s.DeployConfig.WorkingDirectory))

			return ErrSkipDeployment
		}

		if filesChanged {
			s.Log.Debug("changes detected in subdirectory, proceeding with deployment",
				slog.String("directory", s.DeployConfig.WorkingDirectory))
		}
	}

	return nil
}
