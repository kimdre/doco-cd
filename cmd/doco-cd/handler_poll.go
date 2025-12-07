package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"

	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/uuid"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	log "github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/webhook"
)

var (
	ErrNotManagedByDocoCD = errors.New("stack is not managed by doco-cd")
	ErrDeploymentConflict = errors.New("another stack with the same name already exists and is not managed by this repository")
	repoLocks             sync.Map // Map to hold locks for each repository
)

type pollResult struct {
	Metadata notification.Metadata
	Err      error
}

// getRepoLock retrieves a mutex lock for the given repository name.
func getRepoLock(repoName string) *sync.Mutex {
	lockIface, _ := repoLocks.LoadOrStore(repoName, &sync.Mutex{})
	return lockIface.(*sync.Mutex)
}

// StartPoll initializes PollJob with the provided configuration and starts the PollHandler goroutine.
func StartPoll(h *handlerData, pollConfig config.PollConfig, wg *sync.WaitGroup) error {
	if pollConfig.Interval == 0 && !pollConfig.RunOnce {
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

// PollHandler is a function that handles polling for changes in a repository.
func (h *handlerData) PollHandler(pollJob *config.PollJob) {
	repoName := getRepoName(string(pollJob.Config.CloneUrl))

	logger := h.log.With(slog.String("repository", repoName))
	logger.Debug("Start poll handler")

	lock := getRepoLock(repoName)

	for {
		if pollJob.LastRun == 0 || time.Now().Unix() >= pollJob.NextRun {
			locked := lock.TryLock()

			if !locked {
				logger.Warn("Another poll job is still in progress, skipping this run")
			} else {
				metadata := notification.Metadata{
					Repository: repoName,
					Stack:      "",
					Revision:   notification.GetRevision(pollJob.Config.Reference, ""),
					JobID:      uuid.Must(uuid.NewV7()).String(),
				}

				logger.Debug("Start poll job")

				_ = RunPoll(context.Background(), pollJob.Config, h.appConfig, h.dataMountPoint, h.dockerCli, h.dockerClient, logger, metadata, h.secretProvider)

				lock.Unlock()
			}

			pollJob.NextRun = time.Now().Unix() + int64(pollJob.Config.Interval)
		} else {
			logger.Debug("Skipping poll, waiting for next run")
		}

		// If run_once is set, perform a single run and exit after the initial run.
		if pollJob.Config.RunOnce {
			logger.Info("RunOnce configured: single initial poll completed, stopped polling", slog.Any("config", pollJob.Config))
			return
		}

		pollJob.LastRun = time.Now().Unix()
		time.Sleep(time.Duration(pollJob.Config.Interval) * time.Second)
	}
}

func pollError(jobLog *slog.Logger, metadata notification.Metadata, err error) {
	prometheus.PollErrors.WithLabelValues(metadata.Repository).Inc()

	if metadata.Stack != "" {
		jobLog.Error("failed to deploy stack "+metadata.Stack, log.ErrAttr(err))
	} else {
		jobLog.Error("error during poll job", log.ErrAttr(err))
	}

	go func() {
		sendLog := jobLog.With()

		err = notification.Send(notification.Failure, "Poll Job failed", err.Error(), metadata)
		if err != nil {
			sendLog.Error("failed to send notification", log.ErrAttr(err))
		}
	}()
}

// RunPoll deploys compose projects based on the provided configuration.
func RunPoll(ctx context.Context, pollConfig config.PollConfig, appConfig *config.AppConfig, dataMountPoint container.MountPoint,
	dockerCli command.Cli, dockerClient *client.Client, logger *slog.Logger, metadata notification.Metadata, secretProvider *secretprovider.SecretProvider,
) []pollResult {
	var err error

	results := make([]pollResult, 0)

	startTime := time.Now()
	cloneUrl := string(pollConfig.CloneUrl)
	repoName := getRepoName(cloneUrl)
	jobLog := logger.With(slog.String("job_id", metadata.JobID))

	if appConfig.DockerSwarmFeatures {
		// Check if docker host is running in swarm mode
		swarm.ModeEnabled, err = swarm.CheckDaemonIsSwarmManager(ctx, dockerCli)
		if err != nil {
			pollError(jobLog, metadata, fmt.Errorf("failed to check if docker host is running in swarm mode: %w", err))

			return append(results, pollResult{Metadata: metadata, Err: err})
		}
	}

	if strings.Contains(repoName, "..") {
		pollError(jobLog, metadata, fmt.Errorf("invalid repository name: %s, contains '..'", repoName))

		return append(results, pollResult{Metadata: metadata, Err: err})
	}

	if pollConfig.CustomTarget != "" {
		jobLog = jobLog.With(slog.String("custom_target", pollConfig.CustomTarget))
	}

	jobLog.Info("polling repository", slog.Group("trigger", slog.String("event", "poll"), slog.Any("config", pollConfig)))

	jobLog.Debug("get repository",
		slog.String("url", cloneUrl))

	if appConfig.GitAccessToken != "" {
		// Always use the access token for public repositories if it is set to avoid rate limiting
		cloneUrl = git.GetAuthUrl(cloneUrl, appConfig.AuthType, appConfig.GitAccessToken)
	}

	internalTriggerRepoPath, err := filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Destination, repoName), dataMountPoint.Destination) // Path inside the container
	if err != nil {
		pollError(jobLog, metadata, fmt.Errorf("failed to verify and sanitize internal filesystem path: %w", err))

		return append(results, pollResult{Metadata: metadata, Err: err})
	}

	externalTriggerRepoPath, err := filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Source, repoName), dataMountPoint.Source) // Path on the host
	if err != nil {
		pollError(jobLog, metadata, fmt.Errorf("failed to verify and sanitize external filesystem path: %w", err))

		return append(results, pollResult{Metadata: metadata, Err: err})
	}

	jobLog.Debug("cloning repository",
		slog.String("container_path", internalTriggerRepoPath),
		slog.String("host_path", externalTriggerRepoPath))

	_, err = git.CloneRepository(internalTriggerRepoPath, cloneUrl, pollConfig.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy)
	if err != nil {
		// If the repository already exists, check it out to the specified commit SHA
		if errors.Is(err, git.ErrRepositoryAlreadyExists) {
			jobLog.Debug("repository already exists, checking out reference "+pollConfig.Reference, slog.String("host_path", externalTriggerRepoPath))

			_, err = git.UpdateRepository(internalTriggerRepoPath, cloneUrl, pollConfig.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy)
			if err != nil {
				pollError(jobLog, metadata, fmt.Errorf("failed to checkout repository: %w", err))

				return append(results, pollResult{Metadata: metadata, Err: err})
			}
		} else {
			pollError(jobLog, metadata, fmt.Errorf("failed to clone repository: %w", err))

			return append(results, pollResult{Metadata: metadata, Err: err})
		}
	} else {
		jobLog.Debug("repository cloned", slog.String("path", externalTriggerRepoPath))
	}

	jobLog.Debug("retrieving deployment configuration")

	// shortName is the last part of repoName, which is just the name of the repository
	shortName := filepath.Base(repoName)

	// Resolve deployment configs (prefer inline in poll config when present)
	configDir := filepath.Join(internalTriggerRepoPath, appConfig.DeployConfigBaseDir)

	deployConfigs, err := config.ResolveDeployConfigs(pollConfig, configDir, shortName)
	if err != nil {
		pollError(jobLog, metadata, fmt.Errorf("failed to get deploy configuration: %w", err))

		return append(results, pollResult{Metadata: metadata, Err: err})
	}

	err = cleanupObsoleteAutoDiscoveredContainers(ctx, jobLog, dockerClient, dockerCli, string(pollConfig.CloneUrl), deployConfigs, metadata)
	if err != nil {
		pollError(jobLog, metadata, fmt.Errorf("failed to cleanup obsolete auto-discovered containers: %w", err))

		return append(results, pollResult{Metadata: metadata, Err: err})
	}

	for _, deployConfig := range deployConfigs {
		subJobLog := jobLog.With()

		repoName = getRepoName(string(pollConfig.CloneUrl))
		if deployConfig.RepositoryUrl != "" {
			repoName = getRepoName(string(deployConfig.RepositoryUrl))

			// Load all local deployConfig.EnvFiles and load their variables
			err = config.LoadLocalDotEnv(deployConfig, internalTriggerRepoPath)
			if err != nil {
				results = append(results, pollResult{Metadata: metadata, Err: err})
				pollError(subJobLog, metadata, fmt.Errorf("failed to parse local env files: %w", err))

				continue
			}
		}

		metadata.Repository = repoName
		metadata.Stack = deployConfig.Name
		metadata.Revision = notification.GetRevision(deployConfig.Reference, "")

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

		internalDeployRepoPath, err := filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Destination, repoName), dataMountPoint.Destination) // Path inside the container
		if err != nil {
			results = append(results, pollResult{Metadata: metadata, Err: err})
			pollError(subJobLog, metadata, fmt.Errorf("failed to verify and sanitize internal filesystem path: %w", err))

			continue
		}

		externalDeployRepoPath, err := filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Source, repoName), dataMountPoint.Source) // Path on the host
		if err != nil {
			results = append(results, pollResult{Metadata: metadata, Err: err})
			pollError(subJobLog, metadata, fmt.Errorf("failed to verify and sanitize external filesystem path: %w", err))

			continue
		}

		subJobLog = subJobLog.With(
			slog.String("stack", deployConfig.Name),
			slog.String("reference", deployConfig.Reference),
			slog.String("repository", repoName),
		)

		subJobLog.Debug("deployment configuration retrieved", slog.Any("config", deployConfig))

		if deployConfig.RepositoryUrl != "" {
			cloneUrl = string(deployConfig.RepositoryUrl)
			if appConfig.GitAccessToken != "" {
				cloneUrl = git.GetAuthUrl(string(deployConfig.RepositoryUrl), appConfig.AuthType, appConfig.GitAccessToken)
			}

			subJobLog.Debug("repository URL provided, cloning remote repository")
			// Try to clone the remote repository
			_, err = git.CloneRepository(internalDeployRepoPath, cloneUrl, deployConfig.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy)
			if err != nil && !errors.Is(err, git.ErrRepositoryAlreadyExists) {
				results = append(results, pollResult{Metadata: metadata, Err: err})
				pollError(subJobLog, metadata, fmt.Errorf("failed to clone repository: %w", err))

				continue
			}

			subJobLog.Debug("remote repository cloned", slog.String("path", externalDeployRepoPath))
		}

		subJobLog.Debug("checking out reference "+deployConfig.Reference, slog.String("host_path", externalDeployRepoPath))

		repo, err := git.UpdateRepository(internalDeployRepoPath, cloneUrl, deployConfig.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy)
		if err != nil {
			results = append(results, pollResult{Metadata: metadata, Err: err})
			pollError(subJobLog, metadata, fmt.Errorf("failed to checkout repository: %w", err))

			continue
		}

		latestCommit, err := git.GetLatestCommit(repo, deployConfig.Reference)
		if err != nil {
			results = append(results, pollResult{Metadata: metadata, Err: err})
			pollError(subJobLog, metadata, fmt.Errorf("failed to get latest commit: %w", err))

			continue
		}

		metadata.Revision = notification.GetRevision(deployConfig.Reference, latestCommit)

		if deployConfig.Destroy {
			subJobLog.Debug("destroying stack")

			// Check if doco-cd manages the project before destroying the stack
			serviceLabels, err := docker.GetServiceLabels(ctx, dockerClient, deployConfig.Name)
			if err != nil {
				results = append(results, pollResult{Metadata: metadata, Err: err})
				pollError(subJobLog, metadata, fmt.Errorf("failed to retrieve service labels: %w", err))

				continue
			}

			// If no containers are found, skip the destruction step
			if len(serviceLabels) == 0 {
				subJobLog.Debug("no containers found for stack, skipping...")

				continue
			}

			// Check if doco-cd manages the stack
			managed := false
			correctRepo := false

			for _, labels := range serviceLabels {
				if labels[docker.DocoCDLabels.Metadata.Manager] == config.AppName {
					managed = true

					if labels[docker.DocoCDLabels.Repository.Name] == fullName {
						correctRepo = true
					}

					break
				}
			}

			if !managed {
				results = append(results, pollResult{Metadata: metadata, Err: err})
				pollError(subJobLog, metadata, fmt.Errorf("%w: %s: aborting destruction", ErrNotManagedByDocoCD, deployConfig.Name))

				continue
			}

			if !correctRepo {
				results = append(results, pollResult{Metadata: metadata, Err: err})
				pollError(subJobLog, metadata, fmt.Errorf("%w: %s: aborting destruction", ErrDeploymentConflict, deployConfig.Name))

				continue
			}

			err = docker.DestroyStack(subJobLog, &ctx, &dockerCli, deployConfig, metadata)
			if err != nil {
				results = append(results, pollResult{Metadata: metadata, Err: err})
				pollError(subJobLog, metadata, fmt.Errorf("failed to destroy stack: %w", err))

				continue
			}

			if swarm.ModeEnabled && deployConfig.DestroyOpts.RemoveVolumes {
				err = docker.RemoveLabeledVolumes(ctx, dockerClient, deployConfig.Name)
				if err != nil {
					results = append(results, pollResult{Metadata: metadata, Err: err})
					pollError(subJobLog, metadata, fmt.Errorf("failed to remove volumes: %w", err))

					continue
				}
			}

			if deployConfig.DestroyOpts.RemoveRepoDir {
				// Remove the repository directory after destroying the stack
				subJobLog.Debug("removing deployment directory", slog.String("path", externalDeployRepoPath))
				// Check if the parent directory has multiple subdirectories/repos
				parentDir := filepath.Dir(internalDeployRepoPath)

				subDirs, err := os.ReadDir(parentDir)
				if err != nil {
					results = append(results, pollResult{Metadata: metadata, Err: err})
					pollError(subJobLog, metadata, fmt.Errorf("failed to read parent directory: %w", err))

					continue
				}

				if len(subDirs) > 1 {
					// Do not remove the parent directory if it has multiple subdirectories
					subJobLog.Debug("remove deployment directory but keep parent directory as it has multiple subdirectories", slog.String("path", internalDeployRepoPath))

					// Remove only the repository directory
					err = os.RemoveAll(internalDeployRepoPath)
					if err != nil {
						results = append(results, pollResult{Metadata: metadata, Err: err})
						pollError(subJobLog, metadata, fmt.Errorf("failed to remove deployment directory: %w", err))

						continue
					}
				} else {
					// Remove the parent directory if it has only one subdirectory
					err = os.RemoveAll(parentDir)
					if err != nil {
						results = append(results, pollResult{Metadata: metadata, Err: err})
						pollError(subJobLog, metadata, fmt.Errorf("failed to remove deployment directory: %w", err))

						continue
					}

					subJobLog.Debug("removed directory", slog.String("path", parentDir))
				}
			}
		} else {
			// Skip deployment if another project with the same name already exists
			// Check if containers do not belong to this repository or if doco-cd does not manage the stack
			correctRepo := true
			deployedCommit := ""
			deployedSecretHash := ""

			serviceLabels, err := docker.GetServiceLabels(ctx, dockerClient, deployConfig.Name)
			if err != nil {
				results = append(results, pollResult{Metadata: metadata, Err: err})
				pollError(subJobLog, metadata, fmt.Errorf("failed to retrieve service labels: %w", err))

				continue
			}

			for _, labels := range serviceLabels {
				name, ok := labels[docker.DocoCDLabels.Repository.Name]
				if !ok || name != fullName {
					correctRepo = false
					break
				}

				deployedCommit = labels[docker.DocoCDLabels.Deployment.CommitSHA]
				deployedSecretHash = labels[docker.DocoCDLabels.Deployment.ExternalSecretsHash]
			}

			if !correctRepo {
				results = append(results, pollResult{Metadata: metadata, Err: err})
				pollError(subJobLog, metadata, fmt.Errorf("%w: %s: skipping deployment", ErrDeploymentConflict, deployConfig.Name))

				continue
			}

			secretsChanged := false // Flag to indicate if external secrets have changed

			resolvedSecrets := make(secrettypes.ResolvedSecrets)

			if secretProvider != nil && *secretProvider != nil && len(deployConfig.ExternalSecrets) > 0 {
				subJobLog.Debug("resolving external secrets", slog.Any("external_secrets", deployConfig.ExternalSecrets))

				// Resolve external secrets
				resolvedSecrets, err = (*secretProvider).ResolveSecretReferences(ctx, deployConfig.ExternalSecrets)
				if err != nil {
					results = append(results, pollResult{Metadata: metadata, Err: err})
					pollError(subJobLog, metadata, fmt.Errorf("failed to resolve external secrets: %w", err))

					continue
				}

				secretHash := secretprovider.Hash(resolvedSecrets)
				if deployedSecretHash != "" && deployedSecretHash != secretHash {
					subJobLog.Debug("external secrets have changed, proceeding with deployment")

					secretsChanged = true
				}
			}

			subJobLog.Debug("comparing commits",
				slog.String("deployed_commit", deployedCommit),
				slog.String("latest_commit", latestCommit))

			if latestCommit == deployedCommit && !secretsChanged && !deployConfig.ForceImagePull {
				subJobLog.Debug("no new commit found, skipping deployment", slog.String("last_commit", latestCommit))

				continue
			}

			var changedFiles []git.ChangedFile
			if deployedCommit != "" {
				changedFiles, err = git.GetChangedFilesBetweenCommits(repo, plumbing.NewHash(deployedCommit), plumbing.NewHash(latestCommit))
				if err != nil {
					results = append(results, pollResult{Metadata: metadata, Err: err})
					pollError(subJobLog, metadata, fmt.Errorf("failed to get changed files between commits: %w", err))

					continue
				}

				filesChanged, err := git.HasChangesInSubdir(changedFiles, internalDeployRepoPath, deployConfig.WorkingDirectory)
				if err != nil {
					results = append(results, pollResult{Metadata: metadata, Err: err})
					pollError(subJobLog, metadata, fmt.Errorf("failed to compare commits in subdirectory: %w", err))

					continue
				}

				if !filesChanged && !secretsChanged && !deployConfig.ForceImagePull {
					jobLog.Debug("no changes detected in subdirectory, skipping deployment",
						slog.String("directory", deployConfig.WorkingDirectory),
						slog.String("last_commit", latestCommit),
						slog.String("deployed_commit", deployedCommit))

					continue
				}

				if filesChanged {
					subJobLog.Debug("changes detected in subdirectory, proceeding with deployment",
						slog.String("directory", deployConfig.WorkingDirectory),
						slog.String("last_commit", latestCommit),
						slog.String("deployed_commit", deployedCommit))
				}
			}

			payload := webhook.ParsedPayload{
				Ref:       pollConfig.Reference,
				CommitSHA: "poll",
				Name:      shortName,
				FullName:  fullName,
				CloneURL:  string(pollConfig.CloneUrl),
				WebURL:    string(pollConfig.CloneUrl),
			}

			forceDeploy := shouldForceDeploy(deployConfig.Name, latestCommit, appConfig.MaxDeploymentLoopCount)
			if forceDeploy {
				subJobLog.Warn("deployment loop detected for stack, forcing deployment",
					slog.String("stack", deployConfig.Name),
					slog.String("commit", latestCommit))
			}

			err = docker.DeployStack(subJobLog, internalDeployRepoPath, externalDeployRepoPath, &ctx, &dockerCli, dockerClient,
				&payload, deployConfig, changedFiles, latestCommit, config.AppVersion, "poll", forceDeploy, metadata, resolvedSecrets, secretsChanged)
			if err != nil {
				results = append(results, pollResult{Metadata: metadata, Err: err})
				pollError(subJobLog, metadata, fmt.Errorf("failed to deploy stack %s: %w", deployConfig.Name, err))

				continue
			}
		}

		results = append(results, pollResult{Metadata: metadata, Err: nil})
	}

	nextRun := time.Now().Add(time.Duration(pollConfig.Interval) * time.Second).Format(time.RFC3339)
	elapsedTime := time.Since(startTime)

	var hasErrors bool

	for _, result := range results {
		if result.Err != nil {
			hasErrors = true

			break
		}
	}

	if hasErrors {
		jobLog.Warn("job completed with errors", slog.String("elapsed_time", elapsedTime.Truncate(time.Millisecond).String()), slog.String("next_run", nextRun))
	} else {
		jobLog.Info("job completed successfully", slog.String("elapsed_time", elapsedTime.Truncate(time.Millisecond).String()), slog.String("next_run", nextRun))
	}

	prometheus.PollTotal.WithLabelValues(repoName).Inc()
	prometheus.PollDuration.WithLabelValues(repoName).Observe(elapsedTime.Seconds())

	return results
}
