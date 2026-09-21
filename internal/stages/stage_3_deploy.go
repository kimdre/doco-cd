package stages

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker"
)

func (s *StageManager) RunDeployStage(ctx context.Context, stageLog *slog.Logger) error {
	s.Stages.Deploy.StartedAt = time.Now()

	defer func() {
		s.Stages.Deploy.FinishedAt = time.Now()
	}()

	var err error

	// Migrate deployment mode if needed
	if s.DeployState.modeMigrationNeeded {
		_, err = docker.MigrateDeploymentMode(
			ctx,
			stageLog,
			s.Docker.Cmd,
			s.DeployConfig.Context,
			s.DeployConfig.Name,
			s.migrationSource(),
			s.Docker.SwarmMode,
			s.Docker.SwarmAvailable,
		)
		if err != nil {
			return fmt.Errorf("failed to migrate deployment mode: %w", err)
		}
	}

	latestCommit := strings.TrimSpace(s.Repository.Revision)
	if s.Repository.Source != config.SourceTypeOCI {
		latestCommit = s.DeployState.latestCommit
		if latestCommit == "" {
			latestCommit, err = s.latestCommitFromMirror()
			if err != nil {
				return fmt.Errorf("failed to get latest commit: %w", err)
			}
		}
	}

	s.DeployConfig.Internal.ConfigSourceRevision = s.Repository.ConfigRevision
	s.DeployConfig.Internal.ConfigSourceWorkingDir = s.Repository.ConfigPath

	err = docker.DeployStack(ctx, docker.DeployRequest{
		JobLog:           stageLog,
		ExternalRepoPath: s.Repository.PathExternal,
		InternalRepoPath: s.Repository.PathInternal,
		DockerCLI:        s.Docker.Cmd,
		Payload:          s.Payload,
		SourceURL:        sourceURLForLabels(s.Repository),
		DeployConfig:     s.DeployConfig,
		DetectedChanges:  s.DeployState.changedServices,
		NeedSignal:       s.DeployState.ignoredInfo.NeedSendSignal,
		LatestCommit:     latestCommit,
		AppVersion:       app.Version,
		ComposeLoad:      docker.NewComposeLoadOptions(s.AppConfig),
		SwarmRetention:   docker.NewSwarmRetentionOptions(s.AppConfig),
		SwarmMode:        s.Docker.SwarmMode,
		HashNormMap:      pkiRoleNormMap(s.DeployConfig.ExternalSecrets, s.DeployConfig.Internal.Environment),
		Project:          s.Docker.Project,
		ProjectHash:      s.Docker.ProjectHash,
	})
	if err != nil {
		return fmt.Errorf("failed to deploy stack %s: %w", s.DeployConfig.Name, err)
	}

	return nil
}

func sourceURLForLabels(repository *RepositoryData) string {
	if repository == nil {
		return ""
	}

	if repository.ConfigSourceUrl != "" {
		return repository.ConfigSourceUrl
	}

	// Preserve compatibility with callers that construct RepositoryData directly.
	return repository.SourceUrl
}
