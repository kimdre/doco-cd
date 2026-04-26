package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/git"
	log "github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/utils/id"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// StartPoll initializes PollJob with the provided configuration and starts the PollHandler goroutine.
func StartPoll(ctx context.Context, h *handlerData, pollConfig config.PollConfig, wg *sync.WaitGroup) error {
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

	wg.Go(func() {
		h.PollHandler(ctx, pollJob)
		h.log.Debug("PollJob handler stopped", "config", pollConfig)
	})

	return nil
}

// PollHandler is a function that handles polling for changes in a repository.
func (h *handlerData) PollHandler(ctx context.Context, pollJob *config.PollJob) {
	repoName := git.GetRepoName(string(pollJob.Config.CloneUrl))

	logger := h.log.With(slog.String("repository", repoName))
	logger.Debug("Start poll handler")

	repoLock := lock.GetRepoLock(repoName)

	for {
		if pollJob.LastRun == 0 || time.Now().Unix() >= pollJob.NextRun {
			jobID := id.GenID()
			locked := repoLock.TryLock(jobID)

			if !locked {
				logger.Warn("another job is still in progress for this repository",
					slog.String("locked_by_job", repoLock.Holder()),
				)
			} else {
				metadata := notification.Metadata{
					Repository: repoName,
					Stack:      "",
					Revision:   notification.GetRevision(pollJob.Config.Reference, ""),
					JobID:      jobID,
				}

				logger.Debug("start poll job")

				_ = RunPoll(ctx, pollJob.Config, h.appConfig, h.dataMountPoint, h.dockerCli, logger, metadata, h.secretProvider)

				repoLock.Unlock()
			}

			pollJob.NextRun = time.Now().Unix() + int64(pollJob.Config.Interval)
		} else {
			logger.Debug("skipping poll, waiting for next run")
		}

		// If run_once is set, perform a single run and exit after the initial run.
		if pollJob.Config.RunOnce {
			logger.Debug("run_once is set, exiting poll handler after run")
			return
		}

		pollJob.LastRun = time.Now().Unix()

		select {
		case <-ctx.Done():
			logger.Debug("ctx is done in poll handler")
			return
		case <-time.After(time.Duration(pollJob.Config.Interval) * time.Second):
			continue
		}
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
	dockerCli command.Cli, logger *slog.Logger, metadata notification.Metadata, secretProvider *secretprovider.SecretProvider,
) error {
	startTime := time.Now()
	cloneUrl := string(pollConfig.CloneUrl)
	repoName := git.GetRepoName(cloneUrl)
	jobLog := logger.With(slog.String("job_id", metadata.JobID))

	if pollConfig.CustomTarget != "" {
		jobLog = jobLog.With(slog.String("custom_target", pollConfig.CustomTarget))
	}

	jobLog.Info("polling repository",
		slog.Group("trigger",
			slog.String("event", string(stages.JobTriggerPoll)),
			slog.Any("config", pollConfig)))

	deployErr := handle(ctx, jobLog,
		appConfig, dataMountPoint, secretProvider, dockerCli,
		stages.JobTriggerPoll, cloneUrl, pollConfig.Reference, false,
		metadata, pollConfig.CustomTarget, "",
		pollConfig, webhook.ParsedPayload{},
	)

	nextRun := time.Now().Add(time.Duration(pollConfig.Interval) * time.Second).Format(time.RFC3339)
	elapsedTime := time.Since(startTime)

	if deployErr != nil {
		pollError(jobLog, metadata, deployErr)
		jobLog.Warn("job completed with errors", log.ErrAttr(deployErr), slog.String("elapsed_time", elapsedTime.Truncate(time.Millisecond).String()), slog.String("next_run", nextRun))
	} else {
		jobLog.Info("job completed successfully", slog.String("elapsed_time", elapsedTime.Truncate(time.Millisecond).String()), slog.String("next_run", nextRun))
	}

	prometheus.PollTotal.WithLabelValues(repoName).Inc()
	prometheus.PollDuration.WithLabelValues(repoName).Observe(elapsedTime.Seconds())

	return deployErr
}
