package reconciliation

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/utils/id"
)

const reconciliationTraceIDAttr = "doco_cd_reconciliation_trace_id"

// Rewind the Docker events since-cursor slightly to avoid precision edge cases
// around listener restarts and startup recovery.
const reconciliationSinceSafetySkew = 3 * time.Second

func (j *job) run(ctx context.Context) {
	jobLog := j.info.jobLog

	swarmMode := swarm.GetModeEnabled()

	filterArgs := make(client.Filters)
	filterArgs.Add("type", dockerEventTypeForMode(swarmMode))

	if !swarmMode {
		filterArgs.Add("label", docker.DocoCDLabels.Metadata.Manager+"="+config.AppName)

		repositoryLabelValue := gitInternal.GetFullName(string(j.info.repoData.CloneURL))
		if j.info.payload != nil && strings.TrimSpace(j.info.payload.FullName) != "" {
			repositoryLabelValue = j.info.payload.FullName
		}

		filterArgs.Add("label", docker.DocoCDLabels.Repository.Name+"="+repositoryLabelValue)
	}

	for _, eventFilter := range dockerEventFiltersForActions(mapsKeys(j.deployConfigGroupByEvent), swarmMode) {
		filterArgs.Add("event", eventFilter)
	}

	eventSinceCursor := time.Now().UTC().Add(-reconciliationSinceSafetySkew)

	// On job startup, perform a one-time check for already-unhealthy containers
	// so reconciliation can recover them without waiting for a new health event.
	// Run the one-time startup recovery checks in parallel, but wait for both to
	// finish before subscribing to Docker events so all startup healing happens
	// against a stable initial view of the daemon state.
	var startupRecoveryWG sync.WaitGroup

	startupRecoveryWG.Go(func() {
		j.restartUnhealthyContainersOnStartup(ctx, jobLog)
	})

	startupRecoveryWG.Go(func() {
		j.redeployMissingServicesOnStartup(ctx, jobLog)
	})

	startupRecoveryWG.Wait()

	const reconnectDelay = 5 * time.Second

	for {
		// Check exit conditions before (re)connecting.
		select {
		case <-ctx.Done():
			jobLog.Debug("ctx is done")
			return
		case <-j.closeChan:
			jobLog.Debug("channel is closed")
			return
		default:
		}

		listenerCtx, cancel := context.WithCancel(ctx)

		eventResult := j.info.dockerCli.Client().Events(listenerCtx, client.EventsListOptions{
			Filters: filterArgs,
			Since:   dockerEventsSinceValue(eventSinceCursor),
		})

		reconnect, newestEventTime := j.runEventLoop(ctx, jobLog, eventResult.Messages, eventResult.Err)

		if !newestEventTime.IsZero() {
			nextCursor := newestEventTime.UTC().Add(-reconciliationSinceSafetySkew)
			if nextCursor.After(eventSinceCursor) {
				eventSinceCursor = nextCursor
			}
		}

		cancel()

		if !reconnect {
			return
		}

		jobLog.Debug("docker event listener disconnected, reconnecting", slog.Duration("delay", reconnectDelay))

		select {
		case <-ctx.Done():
			jobLog.Debug("ctx is done")
			return
		case <-j.closeChan:
			jobLog.Debug("channel is closed")
			return
		case <-time.After(reconnectDelay):
		}
	}
}

// runEventLoop processes Docker events until the listener disconnects or the job is stopped.
// Returns true when the caller should reconnect, false when it should exit permanently.
func (j *job) runEventLoop(ctx context.Context, jobLog *slog.Logger, eventCh <-chan events.Message, errCh <-chan error) (bool, time.Time) {
	var newestEventTime time.Time

	for {
		select {
		case <-ctx.Done():
			return false, newestEventTime
		case <-j.closeChan:
			return false, newestEventTime
		case err, ok := <-errCh:
			if !ok {
				jobLog.Debug("docker events error channel closed")
				return true, newestEventTime // reconnect
			}

			if err != nil && !errors.Is(err, context.Canceled) {
				jobLog.Error("docker event listener failed", logger.ErrAttr(err))
				return true, newestEventTime // reconnect after error
			}
		case event, ok := <-eventCh:
			if !ok {
				jobLog.Debug("docker events channel closed")
				return true, newestEventTime // reconnect
			}

			eventTime := dockerEventTime(event)
			if eventTime.After(newestEventTime) {
				newestEventTime = eventTime
			}

			j.handleEvent(ctx, jobLog, event)
		}
	}
}

// dockerEventsSinceValue returns a string representation of the given time to be used as the "since" parameter for the Docker events API.
// If the given time is zero, it returns an empty string to indicate no "since" filter.
func dockerEventsSinceValue(cursor time.Time) string {
	if cursor.IsZero() {
		return ""
	}

	return strconv.FormatInt(cursor.UTC().Unix(), 10)
}

func dockerEventTime(event events.Message) time.Time {
	if event.TimeNano > 0 {
		return time.Unix(0, event.TimeNano).UTC()
	}

	if event.Time > 0 {
		return time.Unix(event.Time, 0).UTC()
	}

	return time.Time{}
}

func (j *job) handleEvent(ctx context.Context, jobLog *slog.Logger, event events.Message) {
	action := normalizeReconciliationEventAction(string(event.Action))
	dcs := j.deployConfigGroupByEvent[action]

	if len(dcs) == 0 {
		return
	}

	stackName := stackNameFromEvent(event, dcs)
	if stackName == "" {
		return
	}

	stackDCs := deployConfigsByName(dcs, stackName)
	if len(stackDCs) == 0 {
		return
	}

	if reconciliationHandler.isStackDeploymentInProgress(j.info.metadata.Repository, stackName) {
		jobLog.Debug("suppressing reconciliation event while stack deployment is in progress",
			slog.String("event", action),
			slog.String("stack", stackName),
		)

		return
	}

	if suppress, remaining := j.shouldSuppressRestartFollowupEvent(action, event); suppress {
		jobLog.Debug("suppressing follow-up event from self-initiated container restart",
			slog.String("event", action),
			slog.String("container_name", event.Actor.Attributes["name"]),
			slog.String("restart_cooldown_remaining", remaining.Truncate(time.Second).String()),
			slog.String("stack", stackName),
		)

		return
	}

	stackID := j.info.metadata.Repository + "/" + stackName
	stackLock := lock.GetRepoLock(stackID)

	if !stackLock.TryLock(id.GenID()) {
		jobLog.Debug("skipping reconciliation, already in progress for this stack", slog.String("stack", stackName))
		return
	}
	defer stackLock.Unlock()

	actorGroupName := "container"
	if swarm.GetModeEnabled() {
		actorGroupName = "service"
	}

	traceID := id.GenID()
	event = withReconciliationTraceID(event, traceID)

	eventLog := logger.
		WithoutAttr(jobLog, "job_id").
		With(
			slog.Group("reconciliation",
				slog.String("event", action),
				slog.Group(actorGroupName,
					slog.String("id", shortID(event.Actor.ID)),
					slog.String("name", event.Actor.Attributes["name"]),
				),
				slog.String("trace_id", traceID),
			),
			slog.String("stack", stackName),
		)

	// For restart-oriented events the container is still present — restart it
	// directly instead of going through a full redeploy pipeline.
	if isRestartReconciliationAction(action) {
		restartDC := selectRestartDeployConfig(stackDCs, event.Actor.Attributes)
		if restartDC == nil {
			eventLog.Warn("skipping restart reconciliation, no deploy config matched stack")
			return
		}

		if len(stackDCs) > 1 {
			eventLog.Warn("multiple deploy configs matched restart event, using first match", slog.Int("deploy_config_count", len(stackDCs)))
		}

		restartResult := j.restartContainer(ctx, eventLog, event, restartDC)
		if restartResult.fallbackToDeploy {
			if event.Actor.ID != "" {
				waitForContainerRemovalSettled(ctx, eventLog, j.info.dockerCli.Client(), event.Actor.ID, containerRemovalSettleTimeout)
			}

			j.deploy(ctx, eventLog, stackDCs, action, event, traceID)
		}

		return
	}

	// When the event references a container that is being force-removed, Docker may
	// still report it as "Removing" by the time we begin reconciliation, which causes
	// docker compose to fail with "container is marked for removal and cannot be
	// started". Wait briefly for the container to either be fully removed or settle
	// into a stable state before re-deploying.
	if event.Actor.ID != "" {
		waitForContainerRemovalSettled(ctx, eventLog, j.info.dockerCli.Client(), event.Actor.ID, containerRemovalSettleTimeout)
	}

	j.deploy(ctx, eventLog, stackDCs, action, event, traceID)
}

func (j *job) deploy(ctx context.Context, jobLog *slog.Logger, dcs []*config.DeployConfig, action string, event events.Message, traceID string) {
	repoLock := lock.GetRepoLock(j.info.metadata.Repository)
	repoLock.Lock()
	defer repoLock.Unlock()

	jobLog.Info("reconciliation started")
	defer jobLog.Info("reconciliation completed")

	if err := cleanupObsoleteAutoDiscoveredContainers(ctx, jobLog,
		j.info.dockerCli, string(j.info.repoData.CloneURL),
		j.info.deployConfigs, // all deploy configs
		j.info.metadata); err != nil {
		jobLog.Error("failed to clean up obsolete auto-discovered containers", logger.ErrAttr(err))
	}

	// Reconciliation deploys should always force recreate so missing containers are restored
	// even when there are no Git/compose changes.
	reconcileDCs := cloneDeployConfigsWithForcedRecreate(dcs)

	// Enrich metadata with reconciliation event information for deploy notifications
	actorKind := "container"
	if swarm.GetModeEnabled() {
		actorKind = "service"
	}

	metadata := j.info.metadata
	metadata.ReconciliationEvent = action
	metadata.TraceID = strings.TrimSpace(traceID)
	metadata.AffectedActorKind = actorKind
	metadata.AffectedActorID = shortID(event.Actor.ID)
	metadata.AffectedActorName = strings.TrimSpace(event.Actor.Attributes["name"])

	if err := handleDeploy(ctx, jobLog, j.info.appConfig,
		j.info.dataMountPoint, j.info.dockerCli,
		j.info.secretProvider, metadata.JobID, j.info.jobTrigger,
		j.info.repoData, reconcileDCs, j.info.payload, j.info.testName, metadata); err != nil {
		jobLog.Error("failed to deploy", logger.ErrAttr(err))
	}
}

func withReconciliationTraceID(event events.Message, traceID string) events.Message {
	if strings.TrimSpace(traceID) == "" {
		return event
	}

	attributes := map[string]string{}
	for key, value := range event.Actor.Attributes {
		attributes[key] = value
	}

	attributes[reconciliationTraceIDAttr] = traceID

	event.Actor.Attributes = attributes

	return event
}

func reconciliationTraceIDFromEvent(event events.Message) string {
	if event.Actor.Attributes == nil {
		return ""
	}

	return strings.TrimSpace(event.Actor.Attributes[reconciliationTraceIDAttr])
}
