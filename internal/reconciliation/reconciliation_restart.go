package reconciliation

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
)

// restartContainer restarts a single container identified by the Docker event,
// using the restart timeout configured in the deploy config (default 10 s).
// Used for restart-oriented events where the container is still present.
func (j *job) restartContainer(ctx context.Context, jobLog *slog.Logger, event events.Message, dc *config.DeployConfig) {
	containerID := event.Actor.ID
	containerName := event.Actor.Attributes["name"]
	restartOpts := restartOptionsFromDeployConfig(dc)
	action := normalizeReconciliationEventAction(string(event.Action))

	actorKind := "Container"
	if swarm.GetModeEnabled() {
		actorKind = "Service"
	}

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
		return
	}

	j.markRestartFollowupSuppression(containerID, restartTimeout)

	restartLog.Info("restarting container")

	metadata := notification.Metadata{
		Repository: j.info.metadata.Repository,
		Stack:      j.info.metadata.Stack,
		JobID:      j.info.metadata.JobID,
	}

	if _, err := j.info.dockerCli.Client().ContainerRestart(ctx, containerID, restartOpts); err != nil {
		delete(j.restartSuppressUntil, containerID)
		restartLog.Error("failed to restart container", logger.ErrAttr(err))

		if notifyErr := notification.Send(
			notification.Failure,
			actorKind+" restart failed",
			fmt.Sprintf("%s %s (%s) could not be restarted on %q event: %s", actorKind, containerName, shortID(containerID), action, err.Error()),
			metadata,
		); notifyErr != nil {
			restartLog.Error("failed to send restart failure notification", logger.ErrAttr(notifyErr))
		}

		return
	}

	restartLog.Info("container restarted successfully")

	if notifyErr := notification.Send(
		notification.Success,
		actorKind+" restarted",
		fmt.Sprintf("%s %s (%s) was restarted successfully on %q event", actorKind, containerName, shortID(containerID), action),
		metadata,
	); notifyErr != nil {
		restartLog.Error("failed to send restart success notification", logger.ErrAttr(notifyErr))
	}
}

func (j *job) shouldSuppressRestartFollowupEvent(action string, event events.Message) bool {
	if !isRestartFollowupAction(action) {
		return false
	}

	containerID := event.Actor.ID
	if containerID == "" {
		return false
	}

	until, ok := j.restartSuppressUntil[containerID]
	if !ok {
		return false
	}

	now := time.Now()
	if !now.Before(until) {
		delete(j.restartSuppressUntil, containerID)
		return false
	}

	delete(j.restartSuppressUntil, containerID)

	return true
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

	if notifyErr := notification.Send(
		notification.Warning,
		"Container restart suppressed",
		msg,
		j.info.metadata,
	); notifyErr != nil {
		jobLog.Error("failed to send unhealthy restart suppression notification", logger.ErrAttr(notifyErr))
	}

	return true
}

func restartOptionsFromDeployConfig(dc *config.DeployConfig) client.ContainerRestartOptions {
	timeout := 10
	if dc != nil && dc.Reconciliation.RestartTimeout > 0 {
		timeout = dc.Reconciliation.RestartTimeout
	}

	opts := client.ContainerRestartOptions{Timeout: &timeout}

	if dc != nil {
		signal := strings.TrimSpace(dc.Reconciliation.RestartSignal)
		if signal != "" {
			opts.Signal = signal
		}
	}

	return opts
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
