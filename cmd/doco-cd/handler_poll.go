package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/google/uuid"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	log "github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/webhook"
)

type pollResult struct {
	Metadata notification.Metadata
	Err      error
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
	repoName := stages.GetRepoName(string(pollJob.Config.CloneUrl))

	logger := h.log.With(slog.String("repository", repoName))
	logger.Debug("Start poll handler")

	lock := GetRepoLock(repoName)

	for {
		if pollJob.LastRun == 0 || time.Now().Unix() >= pollJob.NextRun {
			jobID := uuid.Must(uuid.NewV7()).String()
			locked := lock.TryLock(jobID)

			if !locked {
				logger.Warn("another job is still in progress for this repository",
					slog.String("locked_by_job", lock.Holder()),
				)
			} else {
				metadata := notification.Metadata{
					Repository: repoName,
					Stack:      "",
					Revision:   notification.GetRevision(pollJob.Config.Reference, ""),
					JobID:      jobID,
				}

				logger.Debug("start poll job")

				_ = RunPoll(context.Background(), pollJob.Config, h.appConfig, h.dataMountPoint, h.dockerCli, h.dockerClient, logger, metadata, h.secretProvider)

				lock.Unlock()
			}

			pollJob.NextRun = time.Now().Unix() + int64(pollJob.Config.Interval)
		} else {
			logger.Debug("skipping poll, waiting for next run")
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
	repoName := stages.GetRepoName(cloneUrl)
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

	jobLog.Info("polling repository",
		slog.Group("trigger",
			slog.String("event", string(stages.JobTriggerPoll)),
			slog.Any("config", pollConfig)))

	jobLog.Debug("get repository",
		slog.String("url", cloneUrl))

	if appConfig.GitAccessToken != "" && !git.IsSSH(cloneUrl) {
		// Always use the access token for public repositories if it is set to avoid rate limiting
		cloneUrl = git.GetAuthUrl(cloneUrl, appConfig.AuthType, appConfig.GitAccessToken)
	}

	internalRepoPath, err := filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Destination, repoName), dataMountPoint.Destination) // Path inside the container
	if err != nil {
		pollError(jobLog, metadata, fmt.Errorf("failed to verify and sanitize internal filesystem path: %w", err))

		return append(results, pollResult{Metadata: metadata, Err: err})
	}

	externalRepoPath, err := filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPoint.Source, repoName), dataMountPoint.Source) // Path on the host
	if err != nil {
		pollError(jobLog, metadata, fmt.Errorf("failed to verify and sanitize external filesystem path: %w", err))

		return append(results, pollResult{Metadata: metadata, Err: err})
	}

	jobLog.Debug("cloning repository",
		slog.String("container_path", internalRepoPath),
		slog.String("host_path", externalRepoPath))

	_, err = git.CloneRepository(internalRepoPath, cloneUrl, pollConfig.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy, appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase)
	if err != nil {
		// If the repository already exists, check it out to the specified commit SHA
		if errors.Is(err, git.ErrRepositoryAlreadyExists) {
			jobLog.Debug("repository already exists, checking out reference "+pollConfig.Reference, slog.String("host_path", externalRepoPath))

			_, err = git.UpdateRepository(internalRepoPath, cloneUrl, pollConfig.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy, appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase)
			if err != nil {
				pollError(jobLog, metadata, fmt.Errorf("failed to checkout repository: %w", err))

				return append(results, pollResult{Metadata: metadata, Err: err})
			}
		} else {
			pollError(jobLog, metadata, fmt.Errorf("failed to clone repository: %w", err))

			return append(results, pollResult{Metadata: metadata, Err: err})
		}
	} else {
		jobLog.Debug("repository cloned", slog.String("path", externalRepoPath))
	}

	jobLog.Debug("retrieving deployment configuration")

	// shortName is the last part of repoName, which is just the name of the repository
	shortName := filepath.Base(repoName)

	// Resolve deployment configs (prefer inline in poll config when present)
	configDir := filepath.Join(internalRepoPath, appConfig.DeployConfigBaseDir)

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
		deployLog := jobLog.WithGroup("deploy")

		failNotifyFunc := func(err error, metadata notification.Metadata) {
			pollError(deployLog, metadata, err)
		}

		stageMgr := stages.NewStageManager(
			metadata.JobID,
			stages.JobTriggerPoll,
			deployLog,
			failNotifyFunc,
			&stages.RepositoryData{
				CloneURL:     pollConfig.CloneUrl,
				Name:         repoName,
				PathInternal: internalRepoPath,
				PathExternal: externalRepoPath,
			},
			&stages.Docker{
				Cmd:            dockerCli,
				Client:         dockerClient,
				DataMountPoint: dataMountPoint,
			},
			&webhook.ParsedPayload{},
			appConfig,
			deployConfig,
			secretProvider,
		)

		err = stageMgr.RunStages(ctx)
		if err != nil {
			results = append(results, pollResult{Metadata: metadata, Err: err})

			continue
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
