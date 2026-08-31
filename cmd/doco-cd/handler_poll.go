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
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"

	"github.com/kimdre/doco-cd/internal/git"
	log "github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/source/oci"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// Poll trigger reasons, reported in the "polling <entity>" log line's
// trigger.event field so it's possible to tell a regular interval-driven poll
// apart from one triggered by the local repository filesystem watcher.
const (
	pollTriggerDefault = "poll"       // regular interval/startup/API-triggered poll
	pollTriggerWatch   = "poll-watch" // triggered by the local repository filesystem watcher
)

// pollWatcherlessFallbackInterval is used when a local git poll job has no
// configured Interval and either has the watcher disabled or the watcher
// failed to start/closed unexpectedly. It's intentionally long since the job
// is expected to be event-driven, this is just a safety net against never
// polling again.
const pollWatcherlessFallbackInterval = 24 * time.Hour

// StartPoll initializes PollJob with the provided configuration and starts the PollHandler goroutine.
func StartPoll(ctx context.Context, h *handlerData, pollConfig poll.Config, wg *sync.WaitGroup) error {
	isLocalGit := config.NormalizeSourceType(pollConfig.Source) == config.SourceTypeGit &&
		git.IsLocalFile(pollConfig.SourceUrl)

	// interval=0 disables polling, unless the source is a local git repo, which
	// can be driven by a filesystem watcher instead (see pollConfig.Watch).
	if pollConfig.Interval == 0 && !pollConfig.RunOnce && !isLocalGit {
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

	// For local git repositories, start a filesystem watcher so new commits
	// trigger deployment immediately without waiting for the next interval.
	var watchCh <-chan struct{}

	if sourceType == config.SourceTypeGit && git.IsLocalFile(pollJob.Config.SourceUrl) && pollJob.Config.Watch && !pollJob.Config.RunOnce {
		var watchErr error

		watchCh, watchErr = git.WatchLocalGitRef(ctx, pollJob.Config.SourceUrl, logger)
		if watchErr != nil {
			logger.Warn("failed to start local repository watcher, falling back to interval polling",
				log.ErrAttr(watchErr))
		} else {
			logger.Info("watching local repository for changes")
		}
	}

	// If no interval is configured, rely purely on watcher-triggered runs (no
	// periodic fallback at all) as long as the watcher is running. If the
	// watcher failed to start (or is disabled) and no interval is configured,
	// fall back to a long safety-net interval instead of spinning in a tight
	// loop on time.After(0) or never polling again.
	pollInterval := pollJob.Config.Interval
	if pollInterval == 0 && watchCh == nil && !pollJob.Config.RunOnce {
		logger.Warn("no watcher and no poll interval configured, falling back to safety-net poll interval",
			slog.Duration("interval", pollWatcherlessFallbackInterval))

		pollInterval = pollWatcherlessFallbackInterval
	}

	doRun := func(trigger string) {
		logger.Debug("start poll job", slog.String("trigger", trigger))

		triggerReason := pollTriggerDefault
		if trigger == "watch" {
			triggerReason = pollTriggerWatch
		}

		_, _ = h.controlPlaneRuns.RunConfiguredPoll(ctx, pollJob.Config, logger, triggerReason)

		pollJob.LastRun = time.Now().Unix()

		if pollInterval > 0 {
			pollJob.NextRun = time.Now().Add(pollInterval).Unix()
		} else {
			// Watcher-only mode: no periodic run is scheduled.
			pollJob.NextRun = 0
		}
	}

	// Always run immediately on startup.
	doRun("startup")

	if pollJob.Config.RunOnce {
		logger.Debug("run_once is set, exiting poll handler after run")
		return
	}

	// Use a single reusable timer instead of time.After() in the loop below:
	// time.After() allocates a new timer on every iteration that lingers until
	// it fires. When pollInterval is 0 (watcher-only mode), no timer is created
	// at all and timerC stays nil, which permanently disables that select case.
	var (
		timer  *time.Timer
		timerC <-chan time.Time
	)

	if pollInterval > 0 {
		timer = time.NewTimer(pollInterval)
		timerC = timer.C

		defer timer.Stop()
	}

	resetTimer := func(d time.Duration) {
		if d <= 0 {
			if timer != nil {
				timer.Stop()
			}

			timerC = nil

			return
		}

		if timer == nil {
			timer = time.NewTimer(d)
			timerC = timer.C

			return
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		timer.Reset(d)
		timerC = timer.C
	}

	for {
		select {
		case <-ctx.Done():
			logger.Debug("ctx is done in poll handler")
			return

		case <-timerC:
			doRun("interval")
			resetTimer(pollInterval)

		case _, ok := <-watchCh:
			if !ok {
				fallbackInterval, stop := watcherClosedFallback(ctx, logger, pollJob.Config.Interval)
				if stop {
					return
				}

				watchCh = nil
				pollInterval = fallbackInterval

				resetTimer(pollInterval)

				continue
			}

			logger.Debug("local repository watcher detected new commit(s), triggering poll")
			doRun("watch")
			resetTimer(pollInterval)
		}
	}
}

// watcherClosedFallback decides how PollHandler reacts to a closed local
// repository watcher channel. During application shutdown it returns
// stop=true so the handler exits quietly. Otherwise the watcher died
// unexpectedly: it logs a warning and returns the interval to continue
// polling with, falling back to the safety-net interval when no interval is
// configured, so the job keeps running instead of going silent until restart.
func watcherClosedFallback(ctx context.Context, logger *slog.Logger, configuredInterval time.Duration) (time.Duration, bool) {
	if ctx.Err() != nil {
		logger.Debug("ctx is done in poll handler")

		return 0, true
	}

	fallbackInterval := configuredInterval
	if fallbackInterval == 0 {
		fallbackInterval = pollWatcherlessFallbackInterval
	}

	logger.Warn("local repository watcher closed, continuing with interval polling",
		slog.Duration("interval", fallbackInterval))

	return fallbackInterval, false
}

func pollError(jobLog *slog.Logger, metadata notification.Metadata, err error) {
	prometheus.PollErrors.WithLabelValues(metadata.Repository).Inc()

	if metadata.Stack != "" {
		jobLog.Error("failed to deploy stack "+metadata.Stack, log.ErrAttr(err))
	} else {
		jobLog.Error("error during poll job", log.ErrAttr(err))
	}

	// The stack that failed already sent its own notification with the same
	// error text, so a job-level one only says it twice.
	if notification.WasNotified(err) {
		return
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logRecoveredPanic(jobLog, "poll error notification", r)
			}
		}()

		sendLog := jobLog.With()

		err = notification.Send(notification.Failure, "Poll Job failed", err.Error(), metadata)
		if err != nil {
			sendLog.Error("failed to send notification", log.ErrAttr(err))
		}
	}()
}

// RunPoll deploys compose projects based on the provided configuration. triggerReason
// identifies what caused this run (e.g. "poll" for interval/API-triggered runs, or
// "poll-watch" when triggered by the local repository filesystem watcher) and is
// reported in the "polling <entity>" log line's trigger.event field.
func RunPoll(ctx context.Context, pollConfig poll.Config, appConfig *app.Config, dataMountPoint container.MountPoint,
	dockerCli command.Cli, contexts *docker.ContextRegistry, logger *slog.Logger, metadata notification.Metadata, secretProvider *secretprovider.SecretProvider,
	triggerReason string,
) error {
	startTime := time.Now()
	sourceType := config.NormalizeSourceType(pollConfig.Source)
	sourceRef := pollConfig.SourceUrl
	entity := logEntityForSourceType(sourceType)
	sourceURLRewriteApplied := false

	if sourceType == config.SourceTypeGit && appConfig != nil {
		var rewritten string

		rewritten, sourceURLRewriteApplied = rewriteSourceURL(sourceRef, appConfig.SourceURLRewrites)
		sourceRef = config.NormalizeGitURL(rewritten)
	} else if sourceType == config.SourceTypeGit {
		sourceRef = config.NormalizeGitURL(sourceRef)
	}

	repoName := git.GetRepoName(sourceRef)
	if sourceType == config.SourceTypeOCI {
		repoName = oci.RepositoryNameFromArtifact(sourceRef)
	}

	jobLog := logger.With(
		slog.String("job_id", metadata.JobID),
	)

	if sourceURLRewriteApplied {
		jobLog.Debug("using configured source URL rewrite", slog.String("source_url", redactURLUserinfo(sourceRef)))
	}

	if pollConfig.CustomTarget != "" {
		jobLog = jobLog.With(slog.String("target", pollConfig.CustomTarget))
	}

	eventValue := triggerReason
	if eventValue == "" {
		eventValue = pollTriggerDefault
	}

	jobLog.Info("polling "+entity,
		slog.Group("trigger",
			slog.String("event", eventValue),
			slog.Attr{Key: "config", Value: pollConfigLogValue(pollConfig)}))

	// For OCI sources, use the tag from the artifact reference as the deployment reference
	// (e.g., "latest" from "ghcr.io/org/repo:latest") rather than pollConfig.Reference.
	pollReference := pollConfig.Reference
	if sourceType == config.SourceTypeOCI {
		pollReference = oci.TagFromArtifact(sourceRef)
	}

	deployErr := handle(ctx, jobLog,
		appConfig, dataMountPoint, secretProvider, dockerCli, contexts,
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

func pollConfigLogValue(pollConfig poll.Config) slog.Value {
	type deploymentLogValue struct {
		Name string `yaml:"name"`
	}

	type configLogValue struct {
		Source       config.SourceType    `yaml:"source"`
		Reference    string               `yaml:"reference"`
		Interval     time.Duration        `yaml:"interval"`
		CustomTarget string               `yaml:"target"`
		RunOnce      bool                 `yaml:"run_once"`
		Deployments  []deploymentLogValue `yaml:"deployments"`
	}

	deployments := make([]deploymentLogValue, 0, len(pollConfig.Deployments))
	for _, deployment := range pollConfig.Deployments {
		if deployment != nil {
			deployments = append(deployments, deploymentLogValue{Name: deployment.Name})
		}
	}

	value := configLogValue{
		Source:       pollConfig.Source,
		Reference:    pollConfig.Reference,
		Interval:     pollConfig.Interval,
		CustomTarget: pollConfig.CustomTarget,
		RunOnce:      pollConfig.RunOnce,
		Deployments:  deployments,
	}
	if config.NormalizeSourceType(pollConfig.Source) == config.SourceTypeOCI {
		value.Reference = ""
	}

	return log.BuildLogValue(value)
}
