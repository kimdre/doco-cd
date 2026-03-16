package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/logger"

	"github.com/kimdre/doco-cd/internal/utils/set"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
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
	var (
		imagesChanged  bool // Flag to indicate if images have changed
		deployedCommit string
		curProjectHash string
	)

	labels, err := docker.GetLatestServiceLabels(ctx, s.Docker.Client, getFullName(s.Repository.CloneURL), s.DeployConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to get latest labels from deployed services: %w", err)
	}

	if len(labels) > 0 {
		deployedCommit = labels[docker.DocoCDLabels.Deployment.CommitSHA]
		curProjectHash = labels[docker.DocoCDLabels.Deployment.ComposeHash]
	}

	// Compare external secrets if a secret provider is configured
	if s.SecretProvider != nil && *s.SecretProvider != nil && len(s.DeployConfig.ExternalSecrets) > 0 {
		stageLog.Debug("resolving external secrets", slog.Any("external_secrets", s.DeployConfig.ExternalSecrets))

		// Resolve external secrets
		s.DeployState.ResolvedSecrets, err = (*s.SecretProvider).ResolveSecretReferences(ctx, s.DeployConfig.ExternalSecrets)
		if err != nil {
			return fmt.Errorf("failed to resolve external secrets: %w", err)
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

	if deployedCommit != "" {
		// Check for file changes
		s.DeployState.ChangedFiles, err = git.GetChangedFilesBetweenCommits(s.Repository.Git, plumbing.NewHash(deployedCommit), plumbing.NewHash(latestCommit))
		if err != nil {
			return fmt.Errorf("failed to get changed files between commits: %w", err)
		}

		// Validate and sanitize the working directory
		if strings.Contains(s.DeployConfig.WorkingDirectory, "..") {
			return errors.New("invalid working directory: must not contain '..' to prevent directory traversal")
		}

		extAbsWorkingDir, err := getAbsWorkingDir(s.Repository.PathExternal, s.DeployConfig.WorkingDirectory)
		if err != nil {
			return fmt.Errorf("failed to get absolute path of external working directory: %w", err)
		}

		intAbsWorkingDir, err := getAbsWorkingDir(s.Repository.PathInternal, s.DeployConfig.WorkingDirectory)
		if err != nil {
			return fmt.Errorf("failed to get absolute path of internal working directory: %w", err)
		}

		s.DeployConfig.ComposeFiles, err = docker.CheckDefaultComposeFiles(s.DeployConfig.ComposeFiles, intAbsWorkingDir)
		if err != nil {
			return fmt.Errorf("failed to check for default compose files: %w", err)
		}

		// Decrypt any SOPS-encrypted files in the working directory
		f, err := encryption.DecryptFilesInDirectory(s.Repository.PathInternal, intAbsWorkingDir)
		if err != nil {
			return fmt.Errorf("file decryption failed: %w", err)
		}

		if len(f) > 0 {
			s.Log.Debug("decrypted SOPS-encrypted files", slog.Any("files", f))
		}

		// Create a temporary env file if environment variables are specified in the deployment config
		if s.DeployConfig.Internal.Environment != nil {
			tmpEnvFile, err := config.CreateTmpDotEnvFile(s.DeployConfig)
			if err != nil {
				errMsg := "failed to create temporary env file"
				return fmt.Errorf("%s: %w", errMsg, err)
			}

			// Delete the temp file after deployment
			defer func(name string) {
				err = os.Remove(name)
				if err != nil {
					s.Log.Warn("failed to delete temporary env file", logger.ErrAttr(err), slog.String("file", name))
				}
			}(tmpEnvFile)
		}

		s.Docker.Project, err = docker.LoadCompose(
			ctx, extAbsWorkingDir, s.DeployConfig.Name,
			s.DeployConfig.ComposeFiles, s.DeployConfig.EnvFiles,
			s.DeployConfig.Profiles, s.DeployState.ResolvedSecrets)
		if err != nil {
			return fmt.Errorf("failed to load compose project: %w", err)
		}

		newHash, err := docker.ProjectHash(s.Docker.Project)
		if err != nil {
			return fmt.Errorf("failed to get project hash: %w", err)
		}

		composeChanged := newHash != curProjectHash
		if composeChanged {
			stageLog.Debug("compose project has changed, proceeding with deployment", slog.String("new_hash", newHash), slog.String("old_hash", curProjectHash))
		}

		changedFiles, err := docker.ProjectFilesHaveChanges(s.DeployState.ChangedFiles, s.Docker.Project)
		if err != nil {
			return fmt.Errorf("failed to check for changed project files: %s", err)
		}

		if !composeChanged && len(changedFiles) == 0 && !imagesChanged {
			stageLog.Debug("no changes detected, skipping deployment",
				slog.String("directory", s.DeployConfig.WorkingDirectory))

			return ErrSkipDeployment
		}

		stageLog.Debug("changes detected, proceeding with deployment",
			slog.String("directory", s.DeployConfig.WorkingDirectory),
			slog.Group("has_changes",
				slog.Bool("compose_config", composeChanged),
				slog.Any("files", changedFiles),
				slog.Bool("images", imagesChanged),
			),
		)
	}

	return nil
}

// getAbsWorkingDir returns the absolute path of the working directory based on the repository path and the working directory specified in the deployment configuration.
func getAbsWorkingDir(repoPath, workingDir string) (string, error) {
	absPath, err := filepath.Abs(filepath.Join(repoPath, workingDir))
	if err != nil || !strings.HasPrefix(absPath, repoPath) {
		return absPath, errors.New("invalid working directory: resolved path is outside the allowed base directory: " + absPath)
	}

	return absPath, nil
}
