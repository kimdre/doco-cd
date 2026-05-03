package reconciliation

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
)

type restartAttemptResult struct {
	fallbackToDeploy bool
}

// restartContainer restarts a single container identified by the Docker event,
// using the restart timeout configured in the deploy config (default 10 s).
// Used for restart-oriented events where the container is still present.
func (j *job) restartContainer(ctx context.Context, jobLog *slog.Logger, event events.Message, dc *config.DeployConfig) restartAttemptResult {
	containerID := event.Actor.ID
	containerName := event.Actor.Attributes["name"]
	restartOpts := restartOptionsFromDeployConfig(ctx, jobLog, j.info.dockerCli.Client(), containerID, dc)
	action := normalizeReconciliationEventAction(string(event.Action))

	actorKind := restartNotificationActorKind()
	actorKindTitle := restartNotificationActorKindTitle(actorKind)

	restartTimeout := 10
	if restartOpts.Timeout != nil {
		restartTimeout = *restartOpts.Timeout
	}

	restartLog := jobLog.With(
		slog.Int("restart_timeout", restartTimeout),
	)
	if restartOpts.Signal != "" {
		restartLog = restartLog.With(slog.String("restart_signal", restartOpts.Signal))
	}

	if suppressed := j.shouldSuppressUnhealthyRestart(restartLog, event, dc); suppressed {
		return restartAttemptResult{}
	}

	j.markRestartFollowupSuppression(containerID, restartTimeout)

	restartLog.Info("restarting " + actorKind)

	metadata := restartNotificationMetadata(j.info.metadata, action, actorKind, containerID, containerName, reconciliationTraceIDFromEvent(event))

	if _, err := j.info.dockerCli.Client().ContainerRestart(ctx, containerID, restartOpts); err != nil {
		delete(j.restartSuppressUntil, containerID)

		if shouldFallbackToDeployOnRestartError(err) {
			restartLog.Warn("container restart failed because the target is no longer restartable, falling back to redeploy", logger.ErrAttr(err))
			return restartAttemptResult{fallbackToDeploy: true}
		}

		restartLog.Error("failed to restart container", logger.ErrAttr(err))

		if notifyErr := notification.Send(
			notification.Failure,
			actorKindTitle+" restart failed",
			fmt.Sprintf("%s %s (%s) could not be restarted on %q event: %s", actorKindTitle, containerName, shortID(containerID), action, err.Error()),
			metadata,
		); notifyErr != nil {
			restartLog.Error("failed to send restart failure notification", logger.ErrAttr(notifyErr))
		}

		return restartAttemptResult{}
	}

	restartLog.Info(actorKind + " restarted successfully")

	if notifyErr := notification.Send(
		notification.Success,
		actorKindTitle+" restarted",
		fmt.Sprintf("%s %s (%s) was restarted successfully on %q event", actorKindTitle, containerName, shortID(containerID), action),
		metadata,
	); notifyErr != nil {
		restartLog.Error("failed to send restart success notification", logger.ErrAttr(notifyErr))
	}

	return restartAttemptResult{}
}

func shouldFallbackToDeployOnRestartError(err error) bool {
	if err == nil {
		return false
	}

	if errdefs.IsNotFound(err) {
		return true
	}

	errText := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(errText, "no such container") {
		return true
	}

	return strings.Contains(errText, "marked for removal") && strings.Contains(errText, "cannot be started")
}

func restartNotificationMetadata(base notification.Metadata, action, actorKind, actorID, actorName, traceID string) notification.Metadata {
	metadata := base
	metadata.ReconciliationEvent = action
	metadata.TraceID = strings.TrimSpace(traceID)
	metadata.AffectedActorKind = actorKind
	metadata.AffectedActorID = shortID(actorID)
	metadata.AffectedActorName = strings.TrimSpace(actorName)

	return metadata
}

func restartNotificationActorKind() string {
	if swarm.GetModeEnabled() {
		return "service"
	}

	return "container"
}

func restartNotificationActorKindTitle(actorKind string) string {
	if actorKind == "" {
		return ""
	}

	return strings.ToUpper(actorKind[:1]) + actorKind[1:]
}

func (j *job) shouldSuppressRestartFollowupEvent(action string, event events.Message) (bool, time.Duration) {
	if !isRestartFollowupAction(action) {
		return false, 0
	}

	containerID := event.Actor.ID
	if containerID == "" {
		return false, 0
	}

	until, ok := j.restartSuppressUntil[containerID]
	if !ok {
		return false, 0
	}

	now := time.Now()
	if !now.Before(until) {
		delete(j.restartSuppressUntil, containerID)
		return false, 0
	}

	return true, until.Sub(now)
}

func (j *job) markRestartFollowupSuppression(containerID string, timeoutSeconds int) {
	if containerID == "" {
		return
	}

	suppressionWindow := restartFollowupSuppressionWindow(timeoutSeconds)
	j.restartSuppressUntil[containerID] = time.Now().Add(suppressionWindow)
}

func restartFollowupSuppressionWindow(timeoutSeconds int) time.Duration {
	if timeoutSeconds < 0 {
		timeoutSeconds = 0
	}

	// ContainerRestart may block up to the restart timeout before the follow-up die/stop/kill
	// event is processed by this loop, so include the configured timeout plus a small buffer.
	return time.Duration(timeoutSeconds)*time.Second + 10*time.Second
}

func isRestartFollowupAction(action string) bool {
	switch action {
	case "die", "stop", "kill":
		return true
	default:
		return false
	}
}

func (j *job) shouldSuppressUnhealthyRestart(jobLog *slog.Logger, event events.Message, dc *config.DeployConfig) bool {
	action := normalizeReconciliationEventAction(string(event.Action))
	if action != "unhealthy" {
		return false
	}

	containerID := event.Actor.ID
	if containerID == "" || dc == nil {
		return false
	}

	limit := dc.Reconciliation.RestartLimit

	windowSeconds := dc.Reconciliation.RestartWindow
	if limit <= 0 || windowSeconds <= 0 {
		return false
	}

	now := time.Now()
	window := time.Duration(windowSeconds) * time.Second
	history := j.unhealthyRestartHistory[containerID]
	suppressed, updatedHistory := evaluateUnhealthyRestartLimit(history, now, limit, window)
	j.unhealthyRestartHistory[containerID] = updatedHistory

	if !suppressed {
		return false
	}

	msg := fmt.Sprintf("suppressed unhealthy auto-restart after %d restarts in %s", limit, window)
	jobLog.Warn(msg,
		slog.Int("restart_limit", limit),
		slog.Int("restart_window_seconds", windowSeconds),
	)

	actorKind := restartNotificationActorKind()
	metadata := restartNotificationMetadata(j.info.metadata, action, actorKind, containerID, event.Actor.Attributes["name"], reconciliationTraceIDFromEvent(event))

	if notifyErr := notification.Send(
		notification.Warning,
		restartNotificationActorKindTitle(actorKind)+" restart suppressed",
		msg,
		metadata,
	); notifyErr != nil {
		jobLog.Error("failed to send unhealthy restart suppression notification", logger.ErrAttr(notifyErr))
	}

	return true
}

func restartOptionsFromDeployConfig(ctx context.Context, jobLog *slog.Logger, cli client.APIClient, containerID string, dc *config.DeployConfig) client.ContainerRestartOptions {
	timeout := 10
	if dc != nil && dc.Reconciliation.RestartTimeout > 0 {
		timeout = dc.Reconciliation.RestartTimeout
	}

	opts := client.ContainerRestartOptions{Timeout: &timeout}

	signal := ""

	// Priority: user-configured signal → image StopSignal → default SIGINT
	if dc != nil {
		signal = strings.TrimSpace(dc.Reconciliation.RestartSignal)
	}

	if signal == "" && containerID != "" {
		signal = getImageStopSignalForContainer(ctx, jobLog, cli, containerID)
	}

	if signal == "" {
		signal = "SIGINT"
	}

	opts.Signal = signal

	return opts
}

func getImageStopSignalForContainer(ctx context.Context, jobLog *slog.Logger, cli client.APIClient, containerID string) string {
	inspectResult, err := cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		jobLog.Debug("failed to inspect container for image stop signal", slog.String("container_id", shortID(containerID)), logger.ErrAttr(err))
		return ""
	}

	imageRef := strings.TrimSpace(inspectResult.Container.Image)
	if imageRef == "" {
		return ""
	}

	imageInspectResult, err := cli.ImageInspect(ctx, imageRef)
	if err != nil {
		jobLog.Debug("failed to inspect image for stop signal", slog.String("image_ref", imageRef), logger.ErrAttr(err))
		return ""
	}

	config := imageInspectResult.Config
	if config == nil {
		return ""
	}

	stopsignal := strings.TrimSpace(config.StopSignal)

	return stopsignal
}

func selectRestartDeployConfig(dcs []*config.DeployConfig, labels map[string]string) *config.DeployConfig {
	configHash := ""
	if labels != nil {
		configHash = strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.ConfigHash])
	}

	if configHash != "" {
		for _, dc := range dcs {
			if dc == nil {
				continue
			}

			if strings.TrimSpace(dc.Internal.Hash) == configHash {
				return dc
			}
		}
	}

	for _, dc := range dcs {
		if dc != nil {
			return dc
		}
	}

	return nil
}

func evaluateUnhealthyRestartLimit(history []time.Time, now time.Time, limit int, window time.Duration) (bool, []time.Time) {
	if limit <= 0 || window <= 0 {
		return false, history
	}

	pruned := make([]time.Time, 0, len(history)+1)
	for _, ts := range history {
		if now.Sub(ts) < window {
			pruned = append(pruned, ts)
		}
	}

	if len(pruned) >= limit {
		return true, pruned
	}

	pruned = append(pruned, now)

	return false, pruned
}

func isRestartReconciliationAction(action string) bool {
	switch action {
	case "unhealthy", "oom", "kill", "stop":
		return true
	default:
		return false
	}
}
