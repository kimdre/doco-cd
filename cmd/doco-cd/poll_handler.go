package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	log "github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/utils"
	"github.com/kimdre/doco-cd/internal/webhook"
	"golang.org/x/net/context"
)

var (
	ErrNotManagedByDocoCD = errors.New("stack is not managed by doco-cd")
	ErrDeploymentConflict = errors.New("another stack with the same name already exists and is not managed by this repository")
)

// StartPoll initializes PollJob with the provided configuration and starts the PollHandler goroutine.
func StartPoll(h *handlerData, pollConfig config.PollConfig, wg *sync.WaitGroup) error {
	if pollConfig.Interval == 0 {
		h.log.Info("polling job disabled by config", "config", pollConfig)
		return nil
	}

	pollJob := &config.PollJob{
		Config:  pollConfig,
		LastRun: 0,
		NextRun: 0,
	}

	h.log.Debug("Starting poll handler", "config", pollConfig)
	wg.Add(1)

	go func() {
		defer wg.Done()
		h.PollHandler(pollJob)
		h.log.Debug("PollJob handler stopped", "config", pollConfig)
	}()

	return nil
}

// PollHandler is a function that handles polling for changes in a repository
func (h *handlerData) PollHandler(pollJob *config.PollJob) {
	logger := h.log.With()

	logger.Debug("Start poll handler")

	for {
		if pollJob.LastRun == 0 || time.Now().Unix() >= pollJob.NextRun {
			logger.Debug("Running poll for repository")

			_ = RunPoll(context.Background(), pollJob.Config, h.appConfig, h.dataMountPoint, h.dockerCli, h.dockerClient, logger)

			pollJob.NextRun = time.Now().Unix() + int64(pollJob.Config.Interval)
		} else {
			logger.Debug("Skipping poll, waiting for next run")
		}

		pollJob.LastRun = time.Now().Unix()
		time.Sleep(time.Duration(pollJob.Config.Interval) * time.Second)
	}
}

// RunPoll deploys compose projects based on the provided configuration
func RunPoll(ctx context.Context, pollConfig config.PollConfig, appConfig *config.AppConfig, dataMountPoint container.MountPoint, dockerCli command.Cli, dockerClient *client.Client, logger *slog.Logger) error {
	startTime := time.Now()
	cloneUrl := string(pollConfig.CloneUrl)
	jobID := uuid.Must(uuid.NewRandom()).String()
	repoName := getRepoName(cloneUrl)
	jobLog := logger.With(
		slog.String("repository", repoName),
		slog.String("job_id", jobID))

	if strings.Contains(repoName, "..") {
		jobLog.Error("invalid repository name, contains '..'")
		return fmt.Errorf("invalid repository name: %s", repoName)
	}

	if pollConfig.CustomTarget != "" {
		jobLog = jobLog.With(slog.String("custom_target", pollConfig.CustomTarget))
	}

	jobLog.Info("polling repository", slog.Group("trigger", slog.String("event", "poll")))

	jobLog.Debug("get repository",
		slog.String("url", cloneUrl))

	if pollConfig.Private {
		jobLog.Debug("authenticating to private repository")

		if appConfig.GitAccessToken == "" {
			jobLog.Error("missing access token for private repository")
			return fmt.Errorf("missing access token for private repository: %s", repoName)
		}

		cloneUrl = git.GetAuthUrl(cloneUrl, appConfig.AuthType, appConfig.GitAccessToken)
	} else if appConfig.GitAccessToken != "" {
		// Always use the access token for public repositories if it is set to avoid rate limiting
		cloneUrl = git.GetAuthUrl(cloneUrl, appConfig.AuthType, appConfig.GitAccessToken)
	}

	internalRepoPath, err := utils.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Destination, repoName), dataMountPoint.Destination) // Path inside the container
	if err != nil {
		jobLog.Error("failed to verify and sanitize internal filesystem path", log.ErrAttr(err))
		return fmt.Errorf("failed to verify and sanitize internal filesystem path: %w", err)
	}

	externalRepoPath, err := utils.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Destination, repoName), dataMountPoint.Destination) // Path on the host
	if err != nil {
		jobLog.Error("failed to verify and sanitize external filesystem path", log.ErrAttr(err))
		return fmt.Errorf("failed to verify and sanitize external filesystem path: %w", err)
	}

	jobLog.Debug("cloning repository",
		slog.String("container_path", internalRepoPath),
		slog.String("host_path", externalRepoPath))

	_, err = git.CloneRepository(internalRepoPath, cloneUrl, pollConfig.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy)
	if err != nil {
		// If the repository already exists, check it out to the specified commit SHA
		if errors.Is(err, git.ErrRepositoryAlreadyExists) {
			jobLog.Debug("repository already exists, checking out reference "+pollConfig.Reference, slog.String("host_path", externalRepoPath))

			_, err = git.UpdateRepository(internalRepoPath, pollConfig.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy)
			if err != nil {
				jobLog.Error("failed to checkout repository", log.ErrAttr(err))
				return fmt.Errorf("failed to checkout repository: %w", err)
			}
		} else {
			jobLog.Error("failed to clone repository", log.ErrAttr(err))
			return fmt.Errorf("failed to clone repository: %w", err)
		}
	} else {
		jobLog.Debug("repository cloned", slog.String("path", externalRepoPath))
	}

	jobLog.Debug("retrieving deployment configuration")

	// shortName is the last part of repoName, which is just the name of the repository
	shortName := filepath.Base(repoName)

	// Get the deployment configs from the repository
	deployConfigs, err := config.GetDeployConfigs(internalRepoPath, shortName, pollConfig.CustomTarget)
	if err != nil {
		if errors.Is(err, config.ErrDeprecatedConfig) {
			jobLog.Warn(err.Error())
		} else {
			jobLog.Error("failed to get deploy configuration", log.ErrAttr(err))
			return fmt.Errorf("failed to get deploy configuration: %w", err)
		}
	}

	for _, deployConfig := range deployConfigs {
		repoName = getRepoName(string(pollConfig.CloneUrl))
		if deployConfig.RepositoryUrl != "" {
			repoName = getRepoName(string(deployConfig.RepositoryUrl))
		}

		// fullName is the repoName without the domain part,
		// e.g. "github.com/kimdre/doco-cd" becomes "kimdre/doco-cd"
		// or "git.example.com/doco-cd" becomes "doco-cd"
		parts := strings.Split(repoName, "/")
		fullName := repoName

		if len(parts) > 2 {
			fullName = strings.Join(parts[1:], "/")
		} else if len(parts) == 2 {
			fullName = parts[1]
		}

		internalRepoPath, err = utils.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Destination, repoName), dataMountPoint.Destination) // Path inside the container
		if err != nil {
			jobLog.Error("failed to verify and sanitize internal filesystem path", log.ErrAttr(err))
			return fmt.Errorf("failed to verify and sanitize internal filesystem path: %w", err)
		}

		externalRepoPath, err = utils.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Source, repoName), dataMountPoint.Source) // Path on the host
		if err != nil {
			jobLog.Error("failed to verify and sanitize external filesystem path", log.ErrAttr(err))
			return fmt.Errorf("failed to verify and sanitize external filesystem path: %w", err)
		}

		jobLog = jobLog.With(
			slog.String("stack", deployConfig.Name),
			slog.String("reference", deployConfig.Reference),
			slog.String("repository", repoName),
		)

		jobLog.Debug("deployment configuration retrieved", slog.Any("config", deployConfig))

		if deployConfig.RepositoryUrl != "" {
			cloneUrl = string(deployConfig.RepositoryUrl)
			if appConfig.GitAccessToken != "" {
				cloneUrl = git.GetAuthUrl(string(deployConfig.RepositoryUrl), appConfig.AuthType, appConfig.GitAccessToken)
			}

			jobLog.Debug("repository URL provided, cloning remote repository")
			// Try to clone the remote repository
			_, err = git.CloneRepository(internalRepoPath, cloneUrl, deployConfig.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy)
			if err != nil && !errors.Is(err, git.ErrRepositoryAlreadyExists) {
				jobLog.Error("failed to clone repository", log.ErrAttr(err))
				return fmt.Errorf("failed to clone repository: %w", err)
			}

			jobLog.Debug("remote repository cloned", slog.String("path", externalRepoPath))
		}

		jobLog.Debug("checking out reference "+deployConfig.Reference, slog.String("host_path", externalRepoPath))

		repo, err := git.UpdateRepository(internalRepoPath, deployConfig.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy)
		if err != nil {
			jobLog.Error("failed to checkout repository", log.ErrAttr(err))
			return fmt.Errorf("failed to checkout repository: %w", err)
		}

		latestCommit, err := git.GetLatestCommit(repo, deployConfig.Reference)
		if err != nil {
			jobLog.Error("failed to get latest commit", log.ErrAttr(err))
			return fmt.Errorf("failed to get latest commit: %w", err)
		}

		if deployConfig.Destroy {
			jobLog.Debug("destroying stack")

			// Check if doco-cd manages the project before destroying the stack
			containers, err := docker.GetLabeledContainers(ctx, dockerClient, api.ProjectLabel, deployConfig.Name)
			if err != nil {
				jobLog.Error("failed to retrieve containers", log.ErrAttr(err))
				return fmt.Errorf("failed to retrieve containers: %w", err)
			}

			// If no containers are found, skip the destruction step
			if len(containers) == 0 {
				jobLog.Debug("no containers found for stack, skipping...")
				continue
			}

			// Check if doco-cd manages the stack
			managed := false
			correctRepo := false

			for _, cont := range containers {
				if cont.Labels[docker.DocoCDLabels.Metadata.Manager] == config.AppName {
					managed = true

					if cont.Labels[docker.DocoCDLabels.Repository.Name] == fullName {
						correctRepo = true
					}

					break
				}
			}

			if !managed {
				jobLog.Error(fmt.Errorf("%w: %s: aborting destruction", ErrNotManagedByDocoCD, deployConfig.Name).Error())
				return fmt.Errorf("%w: %s: aborting destruction", ErrNotManagedByDocoCD, deployConfig.Name)
			}

			if !correctRepo {
				jobLog.Error(fmt.Errorf("%w: %s: aborting destruction", ErrDeploymentConflict, deployConfig.Name).Error())
				return fmt.Errorf("%w: %s: aborting destruction", ErrDeploymentConflict, deployConfig.Name)
			}

			err = docker.DestroyStack(jobLog, &ctx, &dockerCli, deployConfig)
			if err != nil {
				jobLog.Error("failed to destroy stack", log.ErrAttr(err))
				return fmt.Errorf("failed to destroy stack: %w", err)
			}

			if deployConfig.DestroyOpts.RemoveRepoDir {
				// Remove the repository directory after destroying the stack
				jobLog.Debug("removing deployment directory", slog.String("path", externalRepoPath))
				// Check if the parent directory has multiple subdirectories/repos
				parentDir := filepath.Dir(internalRepoPath)

				subDirs, err := os.ReadDir(parentDir)
				if err != nil {
					jobLog.Error("failed to read parent directory", log.ErrAttr(err))
					return fmt.Errorf("failed to read parent directory: %w", err)
				}

				if len(subDirs) > 1 {
					// Do not remove the parent directory if it has multiple subdirectories
					jobLog.Debug("remove deployment directory but keep parent directory as it has multiple subdirectories", slog.String("path", internalRepoPath))

					// Remove only the repository directory
					err = os.RemoveAll(internalRepoPath)
					if err != nil {
						jobLog.Error("failed to remove deployment directory", log.ErrAttr(err))
						return fmt.Errorf("failed to remove deployment directory: %w", err)
					}
				} else {
					// Remove the parent directory if it has only one subdirectory
					err = os.RemoveAll(parentDir)
					if err != nil {
						jobLog.Error("failed to remove deployment directory", log.ErrAttr(err))
						return fmt.Errorf("failed to remove deployment directory: %w", err)
					}

					jobLog.Debug("removed directory", slog.String("path", parentDir))
				}
			}
		} else {
			// Skip deployment if another project with the same name already exists
			containers, err := docker.GetLabeledContainers(ctx, dockerClient, api.ProjectLabel, deployConfig.Name)
			if err != nil {
				jobLog.Error("failed to retrieve containers", log.ErrAttr(err))
				return fmt.Errorf("failed to retrieve containers: %w", err)
			}

			// Check if containers do not belong to this repository or if doco-cd does not manage the stack
			correctRepo := true
			deployedCommit := ""

			for _, cont := range containers {
				name, ok := cont.Labels[docker.DocoCDLabels.Repository.Name]
				if !ok || name != fullName {
					correctRepo = false
					break
				}

				deployedCommit = cont.Labels[docker.DocoCDLabels.Deployment.CommitSHA]
			}

			if !correctRepo {
				jobLog.Error(fmt.Errorf("%w: %s: skipping deployment", ErrDeploymentConflict, deployConfig.Name).Error())
				return fmt.Errorf("%w: %s: skipping deployment", ErrDeploymentConflict, deployConfig.Name)
			}

			if latestCommit == deployedCommit {
				jobLog.Debug("no changes detected, skipping deployment", slog.String("last_commit", latestCommit))
				continue
			}

			payload := webhook.ParsedPayload{
				Ref:       pollConfig.Reference,
				CommitSHA: "poll",
				Name:      shortName,
				FullName:  fullName,
				CloneURL:  string(pollConfig.CloneUrl),
				WebURL:    string(pollConfig.CloneUrl),
				Private:   pollConfig.Private,
			}

			err = docker.DeployStack(jobLog, internalRepoPath, externalRepoPath, &ctx, &dockerCli, &payload, deployConfig, latestCommit, Version, true)
			if err != nil {
				jobLog.Error("failed to deploy stack", log.ErrAttr(err))
				return fmt.Errorf("failed to deploy stack: %w", err)
			}
		}
	}

	nextRun := time.Now().Add(time.Duration(pollConfig.Interval) * time.Second).Format(time.RFC3339)
	elapsedTime := time.Since(startTime).Truncate(time.Millisecond).String()
	jobLog.Info("job completed successfully", slog.String("elapsed_time", elapsedTime), slog.String("next_run", nextRun))

	return nil
}
