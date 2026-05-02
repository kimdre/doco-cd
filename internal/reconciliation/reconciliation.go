package reconciliation

import (
	"context"
	"errors"
	"log/slog"
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

	// On job startup, perform a one-time check for already-unhealthy containers
	// so reconciliation can recover them without waiting for a new health event.
	// Run the one-time startup recovery checks in parallel, but wait for both to
	// finish before subscribing to Docker events so all startup healing happens
	// against a stable initial view of the daemon state.
	var startupRecoveryWG sync.WaitGroup
	startupRecoveryWG.Add(2)

	go func() {
		defer startupRecoveryWG.Done()

		j.restartUnhealthyContainersOnStartup(ctx, jobLog)
	}()

	go func() {
		defer startupRecoveryWG.Done()

		j.redeployMissingServicesOnStartup(ctx, jobLog)
	}()

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
		})

		reconnect := j.runEventLoop(ctx, jobLog, eventResult.Messages, eventResult.Err)

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
func (j *job) runEventLoop(ctx context.Context, jobLog *slog.Logger, eventCh <-chan events.Message, errCh <-chan error) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-j.closeChan:
			return false
		case err, ok := <-errCh:
			if !ok {
				jobLog.Debug("docker events error channel closed")
				return true // reconnect
			}

			if err != nil && !errors.Is(err, context.Canceled) {
				jobLog.Error("docker event listener failed", logger.ErrAttr(err))
				return true // reconnect after error
			}
		case event, ok := <-eventCh:
			if !ok {
				jobLog.Debug("docker events channel closed")
				return true // reconnect
			}

			j.handleEvent(ctx, jobLog, event)
		}
	}
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

	if j.shouldSuppressRestartFollowupEvent(action, event) {
		jobLog.Debug("suppressing follow-up event from self-initiated container restart",
			slog.String("event", action),
			slog.String("container_name", event.Actor.Attributes["name"]),
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

		j.restartContainer(ctx, eventLog, event, restartDC)

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
