package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/config"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/utils/set"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
)

func shouldSkipDeployment(composeChanged bool,
	changedServices []docker.Change,
	ignoredInfo docker.IgnoredInfo,
	imagesChanged bool,
	mismatchServices []docker.ServiceMismatch,
) bool {
	return !composeChanged &&
		len(changedServices) == 0 &&
		ignoredInfo.IsNeedSignal() &&
		!imagesChanged &&
		len(mismatchServices) == 0
}

func (s *StageManager) RunPreDeployStage(ctx context.Context, stageLog *slog.Logger) error {
	s.Stages.PreDeploy.StartedAt = time.Now()

	defer func() {
		s.Stages.PreDeploy.FinishedAt = time.Now()
	}()

	// Check for external secret changes and current deployed commit
	var (
		imagesChanged bool // Flag to indicate if images have changed
	)

	// Compare external secrets if a secret provider is configured
	if s.SecretProvider != nil && *s.SecretProvider != nil && len(s.DeployConfig.ExternalSecrets) > 0 {
		stageLog.Debug("resolving external secrets", slog.Any("external_secrets", s.DeployConfig.ExternalSecrets))

		encodedSecrets, err := config.EncodeExternalSecretRefs(s.DeployConfig.ExternalSecrets)
		if err != nil {
			return fmt.Errorf("failed to encode external secret references: %w", err)
		}

		// Resolve external secrets
		resolvedSecrets, err := (*s.SecretProvider).ResolveSecretReferences(ctx, encodedSecrets)
		if err != nil {
			return fmt.Errorf("failed to resolve external secrets: %w", err)
		}

		if s.DeployConfig.Internal.Environment == nil {
			s.DeployConfig.Internal.Environment = make(map[string]string)
		}

		maps.Copy(s.DeployConfig.Internal.Environment, resolvedSecrets)
	}

	var err error

	s.DeployConfig.Internal.Hash, err = s.DeployConfig.Hash()
	if err != nil {
		return fmt.Errorf("failed to hash deploy configuration: %w", err)
	}

	if s.DeployConfig.ForceRecreate {
		stageLog.Debug("force recreate enabled, skipping pre-deploy image pull check")
	} else if s.DeployConfig.ForceImagePull {
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

	deployedState, err := docker.GetLatestDeployStatus(ctx, s.Docker.Cmd.Client(), getFullName(s.Repository.CloneURL), s.DeployConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to get latest state from deployed services: %w", err)
	}

	if deployedCommit, _ := deployedState.Labels.GetDeploymentCommitSHA(); deployedCommit != "" {
		latestCommit, err := git.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
		if err != nil {
			return fmt.Errorf("failed to get latest commit: %w", err)
		}

		stageLog.Debug("comparing commits",
			slog.String("deployed_commit", deployedCommit),
			slog.String("latest_commit", latestCommit))

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

		s.Docker.Project, err = docker.LoadCompose(
			ctx, s.Repository.PathExternal, extAbsWorkingDir, s.DeployConfig.Name,
			s.DeployConfig.ComposeFiles, s.DeployConfig.EnvFiles,
			s.DeployConfig.Profiles, s.DeployConfig.Internal.Environment)
		if err != nil {
			return fmt.Errorf("failed to load compose project: %w", err)
		}

		newHash, err := docker.ProjectHash(s.Docker.Project)
		if err != nil {
			return fmt.Errorf("failed to get project hash: %w", err)
		}

		curProjectHash, _ := deployedState.Labels.GetDeploymentComposeHash()

		composeChanged := newHash != curProjectHash
		if composeChanged {
			stageLog.Debug("compose project has changed, proceeding with deployment", slog.String("new_hash", newHash), slog.String("old_hash", curProjectHash))
		}

		// Check for file changes
		gitChangedFiles, err := git.GetChangedFilesBetweenCommits(s.Repository.Git, plumbing.NewHash(deployedCommit), plumbing.NewHash(latestCommit))
		if err != nil {
			return fmt.Errorf("failed to get changed files between commits: %w", err)
		}

		changedFiles := docker.GetPathsFromGitChangedFiles(gitChangedFiles, s.Repository.PathExternal)

		changedServices, ignoredInfo, err := docker.ProjectFilesHaveChanges(s.Repository.PathExternal, changedFiles, s.Docker.Project)
		if err != nil {
			return fmt.Errorf("failed to check for changed project files: %s", err)
		}

		mismatchServices := docker.CheckServiceMismatch(swarm.GetModeEnabled(), deployedState.DeployedStatus, s.Docker.Project.Services)

		if s.DeployConfig.ForceRecreate {
			stageLog.Debug("force recreate enabled, proceeding with deployment",
				slog.String("directory", s.DeployConfig.WorkingDirectory),
			)
		} else if shouldSkipDeployment(composeChanged, changedServices, ignoredInfo, imagesChanged, mismatchServices) {
			stageLog.Debug("no changes detected, skipping deployment",
				slog.String("directory", s.DeployConfig.WorkingDirectory),
			)

			return ErrSkipDeployment
		}

		s.DeployState.changedServices = changedServices
		s.DeployState.ignoredInfo = ignoredInfo

		stageLog.Debug("changes detected, proceeding with deployment",
			slog.String("directory", s.DeployConfig.WorkingDirectory),
			slog.Bool("force_recreate", s.DeployConfig.ForceRecreate),
			slog.Group("has_changes",
				slog.Bool("compose_config", composeChanged),
				slog.Any("files", changedServices),
				slog.Any("ignored_info", ignoredInfo),
				slog.Bool("images", imagesChanged),
				slog.Any("mismatch_services", mismatchServices),
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
