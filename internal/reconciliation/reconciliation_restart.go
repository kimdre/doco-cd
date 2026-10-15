package reconciliation

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
)

type restartAttemptResult struct {
	fallbackToDeploy bool
}

// restartContainer restarts a single container identified by the Docker event,
// using the restart timeout configured in the deploy config (default 10 s).
// Used for restart-oriented events where the container is still present.
func (j *job) restartContainer(ctx context.Context, jobLog *slog.Logger, event events.Message, dc *deployConfig.Config, cli command.Cli, swarmMode bool) restartAttemptResult {
	containerID := event.Actor.ID
	containerName := event.Actor.Attributes["name"]
	restartOpts := restartOptionsFromDeployConfig(dc)
	action := normalizeReconciliationEventAction(string(event.Action))

	actorKind := restartNotificationActorKind(swarmMode)
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

	if suppressed := j.shouldSuppressUnhealthyRestart(restartLog, event, dc, swarmMode); suppressed {
		return restartAttemptResult{}
	}

	// Swarm does not support Compose lifecycle hooks.
	var (
		hookService types.ServiceConfig
		hasHooks    bool
	)

	if !swarmMode {
		hookService, hasHooks = j.restartHookService(ctx, restartLog, cli, event.Actor.Attributes)
	}

	if hasHooks {
		// The pre_stop hook may fail because the container is unhealthy. Restart the container anyway,
		// because a restart is the recovery. A stopped container has nothing to prepare for the stop.
		if err := runReconciliationHooks(ctx, cli, containerID, hookService, docker.ComposeHookPreStop); err != nil &&
			!docker.IsComposeHookContainerUnavailable(err) {
			restartLog.Warn("pre_stop hook failed, restarting the "+actorKind+" anyway", logger.ErrAttr(err))
		}
	}

	// Mark after the pre_stop hook, so a slow hook does not use up the suppression window.
	j.markRestartFollowupSuppression(containerID, restartTimeout)

	restartLog.Info("restarting " + actorKind)

	metadata := restartNotificationMetadata(j.requestFor(dc).Metadata, dc, action, actorKind, containerID, containerName, reconciliationTraceIDFromEvent(event))

	if _, err := cli.Client().ContainerRestart(ctx, containerID, restartOpts); err != nil {
		j.restartStateMu.Lock()
		delete(j.restartSuppressUntil, containerID)
		j.restartStateMu.Unlock()

		if shouldFallbackToDeployOnRestartError(err) {
			restartLog.Warn("container restart failed because the target is no longer restartable, falling back to redeploy", logger.ErrAttr(err))
			return restartAttemptResult{fallbackToDeploy: true}
		}

		restartLog.Error("failed to restart container", logger.ErrAttr(err))

		if notifyErr := j.manager.notifier.Send(
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

	if hasHooks {
		hookErr := runReconciliationHooks(ctx, cli, containerID, hookService, docker.ComposeHookPostStart)

		// Events of the restart wait in the queue while the hook runs. Renew the window, so they stay suppressed.
		j.markRestartFollowupSuppression(containerID, restartTimeout)

		if hookErr != nil {
			restartLog.Error("post_start hook failed after the "+actorKind+" restart", logger.ErrAttr(hookErr))

			if notifyErr := j.manager.notifier.Send(
				notification.Warning,
				actorKindTitle+" restarted, post_start hook failed",
				fmt.Sprintf("%s %s (%s) was restarted on %q event, but its post_start hook failed: %s",
					actorKindTitle, containerName, shortID(containerID), action, hookErr.Error()),
				metadata,
			); notifyErr != nil {
				restartLog.Error("failed to send restart hook failure notification", logger.ErrAttr(notifyErr))
			}

			return restartAttemptResult{}
		}
	}

	if notifyErr := j.manager.notifier.Send(
		notification.Success,
		actorKindTitle+" restarted",
		fmt.Sprintf("%s %s (%s) was restarted successfully on %q event", actorKindTitle, containerName, shortID(containerID), action),
		metadata,
	); notifyErr != nil {
		restartLog.Error("failed to send restart success notification", logger.ErrAttr(notifyErr))
	}

	return restartAttemptResult{}
}

// reconciliationHookTimeout limits the project load and each group of lifecycle hooks during a reconciliation restart.
// Compose has no hook timeout, but a hanging hook or secret provider must not block the event loop of the repository.
const reconciliationHookTimeout = 2 * time.Minute

// restartHookService loads the deployed Compose service definition of a container for its lifecycle hooks.
// It returns false if the service has no pre_stop or post_start hooks, or if the definition cannot be loaded.
func (j *job) restartHookService(ctx context.Context, log *slog.Logger, cli command.Cli, labels map[string]string) (types.ServiceConfig, bool) {
	// The load resolves external secrets. Skip it if the labels show that the service has no restart hooks.
	if !docker.MayHaveComposeHooks(labels, docker.ComposeHookPreStop, docker.ComposeHookPostStart) {
		return types.ServiceConfig{}, false
	}

	loadCtx, cancel := context.WithTimeout(ctx, reconciliationHookTimeout)
	defer cancel()

	var (
		svc types.ServiceConfig
		ok  bool
	)

	if j.manager.loadHookService != nil {
		svc, ok = j.manager.loadHookService(loadCtx, log, cli, labels)
	} else {
		loader := &docker.ComposeHookLoader{
			SecretProvider: j.manager.secretProvider,
			Options:        docker.NewScheduledComposeOptions(j.manager.appConfig),
			Logger:         log,
		}
		svc, ok = loader.HookService(loadCtx, cli, labels)
	}

	if !ok {
		return types.ServiceConfig{}, false
	}

	return svc, docker.HasComposeHooks(svc, docker.ComposeHookPreStop) || docker.HasComposeHooks(svc, docker.ComposeHookPostStart)
}

// runReconciliationHooks runs the pre_stop or post_start hooks of a Compose service for a container.
func runReconciliationHooks(ctx context.Context, cli command.Cli, containerID string, svc types.ServiceConfig, phase docker.ComposeHookPhase) error {
	hookCtx, cancel := context.WithTimeout(ctx, reconciliationHookTimeout)
	defer cancel()

	return docker.RunComposeHooks(hookCtx, cli.Client(), containerID, svc, phase)
}

// shouldFallbackToDeployOnRestartError returns true if the error indicates that the container is no longer restartable.
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

// restartNotificationMetadata creates a notification.Metadata for a restart event.
func restartNotificationMetadata(base notification.Metadata, dc *deployConfig.Config, action, actorKind, actorID, actorName, traceID string) notification.Metadata {
	metadata := base
	if dc != nil {
		metadata.Context = dc.Context
		metadata.Target = dc.Internal.ConfigTarget
	}

	metadata.ReconciliationEvent = action
	metadata.TraceID = strings.TrimSpace(traceID)
	metadata.AffectedActorKind = actorKind
	metadata.AffectedActorID = shortID(actorID)
	metadata.AffectedActorName = strings.TrimSpace(actorName)

	return metadata
}

// restartNotificationActorKind returns the actor kind for a restart notification.
func restartNotificationActorKind(swarmMode bool) string {
	if swarmMode {
		return "service"
	}

	return "container"
}

// restartNotificationActorKindTitle returns the title-cased actor kind for a restart notification.
func restartNotificationActorKindTitle(actorKind string) string {
	if actorKind == "" {
		return ""
	}

	return strings.ToUpper(actorKind[:1]) + actorKind[1:]
}

// shouldSuppressRestartFollowupEvent returns true if the event is a follow-up die/stop/kill event of a container that was just restarted.
func (j *job) shouldSuppressRestartFollowupEvent(action string, event events.Message) (bool, time.Duration) {
	if !isRestartFollowupAction(action) {
		return false, 0
	}

	containerID := event.Actor.ID
	if containerID == "" {
		return false, 0
	}

	j.restartStateMu.Lock()
	defer j.restartStateMu.Unlock()

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

// markRestartFollowupSuppression marks a container's follow-up die/stop/kill events to be suppressed for a certain duration.
func (j *job) markRestartFollowupSuppression(containerID string, timeoutSeconds int) {
	if containerID == "" {
		return
	}

	suppressionWindow := restartFollowupSuppressionWindow(timeoutSeconds)

	j.restartStateMu.Lock()
	j.restartSuppressUntil[containerID] = time.Now().Add(suppressionWindow)
	j.restartStateMu.Unlock()
}

// restartFollowupSuppressionWindow returns the duration for which follow-up die/stop/kill events should be suppressed.
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

func (j *job) shouldSuppressUnhealthyRestart(jobLog *slog.Logger, event events.Message, dc *deployConfig.Config, swarmMode bool) bool {
	action := normalizeReconciliationEventAction(string(event.Action))
	if action != "unhealthy" {
		return false
	}

	containerID := event.Actor.ID
	if containerID == "" || dc == nil {
		return false
	}

	limit := dc.Reconciliation.RestartLimit

	window := dc.Reconciliation.RestartWindow.Duration()
	if limit <= 0 || window <= 0 {
		return false
	}

	windowSeconds := int64(window / time.Second)
	now := time.Now()

	j.restartStateMu.Lock()
	history := j.unhealthyRestartHistory[containerID]
	suppressed, updatedHistory := evaluateUnhealthyRestartLimit(history, now, limit, window)
	j.unhealthyRestartHistory[containerID] = updatedHistory
	j.restartStateMu.Unlock()

	if !suppressed {
		return false
	}

	msg := fmt.Sprintf("suppressed unhealthy auto-restart after %d restarts in %s", limit, window)
	jobLog.Warn(msg,
		slog.Int("restart_limit", limit),
		slog.Int64("restart_window_seconds", windowSeconds),
	)

	actorKind := restartNotificationActorKind(swarmMode)
	metadata := restartNotificationMetadata(j.requestFor(dc).Metadata, dc, action, actorKind, containerID, event.Actor.Attributes["name"], reconciliationTraceIDFromEvent(event))

	if notifyErr := j.manager.notifier.Send(
		notification.Warning,
		restartNotificationActorKindTitle(actorKind)+" restart suppressed",
		msg,
		metadata,
	); notifyErr != nil {
		jobLog.Error("failed to send unhealthy restart suppression notification", logger.ErrAttr(notifyErr))
	}

	return true
}

func restartOptionsFromDeployConfig(dc *deployConfig.Config) client.ContainerRestartOptions {
	timeout := 10
	if dc != nil && dc.Reconciliation.RestartTimeout > 0 {
		timeout = dc.Reconciliation.RestartTimeout.Seconds()
	}

	opts := client.ContainerRestartOptions{Timeout: &timeout}

	if dc != nil {
		opts.Signal = strings.TrimSpace(dc.Reconciliation.RestartSignal)
	}

	return opts
}

func selectRestartDeployConfig(dcs []*deployConfig.Config, labels map[string]string) *deployConfig.Config {
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
