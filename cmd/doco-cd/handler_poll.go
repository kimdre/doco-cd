package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"

	"github.com/kimdre/doco-cd/internal/git"
	log "github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/source/oci"
	"github.com/kimdre/doco-cd/internal/utils/id"
	"github.com/kimdre/doco-cd/internal/webhook"
)

type pollRunner func(ctx context.Context, pollConfig poll.Config, appConfig *app.Config, dataMountPoint container.MountPoint,
	dockerCli command.Cli, logger *slog.Logger, metadata notification.Metadata, secretProvider *secretprovider.SecretProvider,
) error

// StartPoll initializes PollJob with the provided configuration and starts the PollHandler goroutine.
func StartPoll(ctx context.Context, h *handlerData, pollConfig poll.Config, wg *sync.WaitGroup) error {
	if pollConfig.Interval == 0 && !pollConfig.RunOnce {
		h.log.Info("polling job disabled by config", "config", &pollConfig)

		return nil
	}

	pollJob := &poll.Job{
		Config:  pollConfig,
		LastRun: 0,
		NextRun: 0,
	}

	h.log.Debug("Starting poll handler", "config", &pollConfig)

	wg.Go(func() {
		h.PollHandler(ctx, pollJob)
		h.log.Debug("PollJob handler stopped", "config", &pollConfig)
	})

	return nil
}

// PollHandler handles polling for changes in a configured source.
func (h *handlerData) PollHandler(ctx context.Context, pollJob *poll.Job) {
	sourceType := config.NormalizeSourceType(pollJob.Config.Source)
	entity := logEntityForSourceType(sourceType)

	repoName := git.GetRepoName(pollJob.Config.SourceUrl)
	if sourceType == config.SourceTypeOCI {
		repoName = oci.RepositoryNameFromArtifact(pollJob.Config.SourceUrl)
	}

	logValue := repoName
	if sourceType == config.SourceTypeOCI {
		logValue = pollJob.Config.SourceUrl
	}

	logger := h.log.With(slog.String(entity, logValue))
	logger.Debug("Start poll handler")

	runner := h.runPoll
	if runner == nil {
		runner = RunPoll
	}

	for {
		if pollJob.LastRun == 0 || time.Now().Unix() >= pollJob.NextRun {
			jobID := id.GenID()

			metadata := notification.Metadata{
				Repository: repoName,
				Stack:      "",
				Revision:   notification.GetRevision(pollJob.Config.Reference, ""),
				JobID:      jobID,
			}

			logger.Debug("start poll job")

			_ = runner(ctx, pollJob.Config, h.appConfig, h.dataMountPoint, h.dockerCli, logger, metadata, h.secretProvider)

			pollJob.NextRun = time.Now().Add(pollJob.Config.Interval).Unix()
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
		case <-time.After(pollJob.Config.Interval):
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
		defer recoverPanic(jobLog, "poll error notification")

		sendLog := jobLog.With()

		err = notification.Send(notification.Failure, "Poll Job failed", err.Error(), metadata)
		if err != nil {
			sendLog.Error("failed to send notification", log.ErrAttr(err))
		}
	}()
}

// RunPoll deploys compose projects based on the provided configuration.
func RunPoll(ctx context.Context, pollConfig poll.Config, appConfig *app.Config, dataMountPoint container.MountPoint,
	dockerCli command.Cli, logger *slog.Logger, metadata notification.Metadata, secretProvider *secretprovider.SecretProvider,
) error {
	startTime := time.Now()
	sourceType := config.NormalizeSourceType(pollConfig.Source)
	sourceRef := pollConfig.SourceUrl
	entity := logEntityForSourceType(sourceType)

	repoName := git.GetRepoName(sourceRef)
	if sourceType == config.SourceTypeOCI {
		repoName = oci.RepositoryNameFromArtifact(sourceRef)
	}

	jobLog := logger.With(
		slog.String("job_id", metadata.JobID),
	)

	if pollConfig.CustomTarget != "" {
		jobLog = jobLog.With(slog.String("target", pollConfig.CustomTarget))
	}

	configVal := log.BuildLogValue(&pollConfig, "Deployments.Internal")
	if pollConfig.Source == config.SourceTypeOCI {
		configVal = log.BuildLogValue(&pollConfig, "Reference", "Deployments.Internal")
	}

	jobLog.Info("polling "+entity,
		slog.Group("trigger",
			slog.String("event", string(stages.JobTriggerPoll)),
			slog.Attr{Key: "config", Value: configVal}))

	// For OCI sources, use the tag from the artifact reference as the deployment reference
	// (e.g., "latest" from "ghcr.io/org/repo:latest") rather than pollConfig.Reference.
	pollReference := pollConfig.Reference
	if sourceType == config.SourceTypeOCI {
		pollReference = oci.TagFromArtifact(sourceRef)
	}

	deployErr := handle(ctx, jobLog,
		appConfig, dataMountPoint, secretProvider, dockerCli,
		stages.JobTriggerPoll, sourceType, sourceRef, pollReference, false,
		metadata, pollConfig.CustomTarget, "",
		pollConfig, webhook.ParsedPayload{},
	)

	nextRun := time.Now().Add(pollConfig.Interval).Format(time.RFC3339)
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
