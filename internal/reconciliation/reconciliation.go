package reconciliation

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"

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

// contextualEvent pairs a Docker daemon event with the context name it originated from.
type contextualEvent struct {
	event       events.Message
	contextName string
}

// initContextCLIs populates j.contextCLIs with a Docker CLI entry for the default context
// and for every unique non-default context referenced in the job's deploy configs.
func (j *job) initContextCLIs(ctx context.Context, quiet bool) {
	contextCLIs := map[string]contextCLIEntry{
		"": {cli: j.info.dockerCli, swarmMode: swarm.GetModeEnabled()},
	}

	for _, dc := range j.info.deployConfigs {
		ctxName := strings.TrimSpace(dc.Context)
		if ctxName == "" {
			continue
		}

		if _, already := contextCLIs[ctxName]; already {
			continue
		}

		cli, closeFn, err := dockerCliForContext(j.info.dockerCli, quiet, ctxName)
		if err != nil {
			j.info.jobLog.Error("failed to create Docker CLI for context; skipping event listener for that context",
				slog.String("context", ctxName),
				logger.ErrAttr(err),
			)

			continue
		}

		swarmMode, err := swarm.ResolveModeEnabled(ctx, cli.Client())
		if err != nil {
			j.info.jobLog.Warn("failed to determine swarm mode for context, assuming non-swarm",
				slog.String("context", ctxName),
				logger.ErrAttr(err),
			)
		}

		contextCLIs[ctxName] = contextCLIEntry{cli: cli, closeFn: closeFn, swarmMode: swarmMode}
	}

	j.contextCLIs = contextCLIs
}

// cliForContext returns the Docker CLI for the given context name, falling back to the
// default CLI if the context is not found.
func (j *job) cliForContext(contextName string) command.Cli {
	contextName = strings.TrimSpace(contextName)
	if j.contextCLIs != nil {
		if e, ok := j.contextCLIs[contextName]; ok {
			return e.cli
		}
	}

	return j.info.dockerCli
}

// swarmModeForContext returns the swarm mode for the given context name, falling back to the
// globally cached value for the default context.
func (j *job) swarmModeForContext(contextName string) bool {
	contextName = strings.TrimSpace(contextName)
	if j.contextCLIs != nil {
		if e, ok := j.contextCLIs[contextName]; ok {
			return e.swarmMode
		}
	}

	return swarm.GetModeEnabled()
}

// deployConfigsForContext returns the subset of the job's deploy configs that target contextName.
func (j *job) deployConfigsForContext(contextName string) []*deployConfig.Config {
	return filterConfigsByContext(j.info.deployConfigs, contextName)
}

func (j *job) run(ctx context.Context) {
	jobLog := j.info.jobLog

	dockerQuiet := false
	if j.info.appConfig != nil {
		dockerQuiet = j.info.appConfig.DockerQuietDeploy
	}

	j.initContextCLIs(ctx, dockerQuiet)

	// Wait for all event-listener goroutines to exit before closing their Docker
	// CLIs, so we never close a client that a listener is still using.
	var listenerWG sync.WaitGroup

	defer func() {
		listenerWG.Wait()

		for ctxName, entry := range j.contextCLIs {
			if ctxName != "" && entry.closeFn != nil {
				entry.closeFn()
			}
		}
	}()

	// Startup recovery: run for every configured context in parallel.
	// Run both checks concurrently per context, then wait for all to finish
	// before subscribing to Docker events so startup healing happens against
	// a stable initial view of the daemon state.
	var startupRecoveryWG sync.WaitGroup

	for ctxName, entry := range j.contextCLIs {
		startupRecoveryWG.Add(2)

		go func(ctxName string, entry contextCLIEntry) {
			defer startupRecoveryWG.Done()

			j.restartUnhealthyContainersOnStartup(ctx, jobLog, ctxName, entry.cli, entry.swarmMode)
		}(ctxName, entry)

		go func(ctxName string, entry contextCLIEntry) {
			defer startupRecoveryWG.Done()

			j.redeployMissingServicesOnStartup(ctx, jobLog, ctxName, entry.cli, entry.swarmMode)
		}(ctxName, entry)
	}

	startupRecoveryWG.Wait()

	// Fan-in Docker events from all contexts into a single channel processed serially.
	// The buffer absorbs short bursts from multiple daemons without backpressure.
	mergedCh := make(chan contextualEvent, 256)

	for ctxName, entry := range j.contextCLIs {
		listenerWG.Add(1)

		go func(ctxName string, entry contextCLIEntry) {
			defer listenerWG.Done()

			j.runContextEventListener(ctx, jobLog, ctxName, entry, mergedCh)
		}(ctxName, entry)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-j.closeChan:
			return
		case ce, ok := <-mergedCh:
			if !ok {
				return
			}

			j.handleEvent(ctx, jobLog, ce.event, ce.contextName)
		}
	}
}

// runContextEventListener connects to the Docker daemon for entry, listens for relevant events,
// forwards them (tagged with contextName) to out, and automatically reconnects on disconnection.
func (j *job) runContextEventListener(ctx context.Context, jobLog *slog.Logger, contextName string, entry contextCLIEntry, out chan<- contextualEvent) {
	repositoryLabelValue := gitInternal.GetFullName(j.info.repoData.SourceUrl)
	if j.info.payload != nil && strings.TrimSpace(j.info.payload.FullName) != "" {
		repositoryLabelValue = j.info.payload.FullName
	}

	swarmMode := entry.swarmMode

	// Only listen for events for configs that target this context.
	contextDCs := j.deployConfigsForContext(contextName)
	contextGroupByEvent := getDeployConfigGroupByEvent(contextDCs)

	if len(contextGroupByEvent) == 0 {
		return
	}

	filterArgs := make(client.Filters)
	filterArgs.Add("type", dockerEventTypeForMode(swarmMode))

	if !swarmMode {
		filterArgs.Add("label", docker.DocoCDLabels.Metadata.Manager+"="+app.Name)
		filterArgs.Add("label", docker.DocoCDLabels.Source.Name+"="+repositoryLabelValue)
	}

	for _, eventFilter := range dockerEventFiltersForActions(mapsKeys(contextGroupByEvent), swarmMode) {
		filterArgs.Add("event", eventFilter)
	}

	eventSinceCursor := time.Now().UTC().Add(-reconciliationSinceSafetySkew)

	const reconnectDelay = 5 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		case <-j.closeChan:
			return
		default:
		}

		listenerCtx, cancel := context.WithCancel(ctx)

		eventResult := entry.cli.Client().Events(listenerCtx, client.EventsListOptions{
			Filters: filterArgs,
			Since:   dockerEventsSinceValue(eventSinceCursor),
		})

		reconnect, newestEventTime := j.forwardEvents(ctx, jobLog, eventResult.Messages, eventResult.Err, contextName, out)

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

		if contextName == "" {
			jobLog.Debug("docker event listener disconnected, reconnecting", slog.Duration("delay", reconnectDelay))
		} else {
			jobLog.Debug("docker event listener disconnected, reconnecting",
				slog.String("context", contextName),
				slog.Duration("delay", reconnectDelay),
			)
		}

		select {
		case <-ctx.Done():
			return
		case <-j.closeChan:
			return
		case <-time.After(reconnectDelay):
		}
	}
}

// forwardEvents reads events from the Docker streaming API and sends them (tagged with contextName)
// to out. Returns (reconnect bool, newestEventTime).
func (j *job) forwardEvents(ctx context.Context, jobLog *slog.Logger, eventCh <-chan events.Message, errCh <-chan error, contextName string, out chan<- contextualEvent) (bool, time.Time) {
	var newestEventTime time.Time

	for {
		select {
		case <-ctx.Done():
			return false, newestEventTime
		case <-j.closeChan:
			return false, newestEventTime
		case err, ok := <-errCh:
			if !ok {
				jobLog.Debug("docker events error channel closed", slog.String("context", contextName))
				return true, newestEventTime // reconnect
			}

			if err != nil && !errors.Is(err, context.Canceled) {
				jobLog.Error("docker event listener failed", slog.String("context", contextName), logger.ErrAttr(err))
				return true, newestEventTime // reconnect after error
			}
		case event, ok := <-eventCh:
			if !ok {
				jobLog.Debug("docker events channel closed", slog.String("context", contextName))
				return true, newestEventTime // reconnect
			}

			eventTime := dockerEventTime(event)
			if eventTime.After(newestEventTime) {
				newestEventTime = eventTime
			}

			select {
			case out <- contextualEvent{event: event, contextName: contextName}:
			case <-ctx.Done():
				return false, newestEventTime
			case <-j.closeChan:
				return false, newestEventTime
			}
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

func (j *job) handleEvent(ctx context.Context, jobLog *slog.Logger, event events.Message, contextName string) {
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

	// Skip reconciliation if all matching configs have destroy enabled
	// to prevent attempting to redeploy stacks that are being destroyed
	allDestroyEnabled := true

	for _, dc := range stackDCs {
		if !dc.Destroy.Enabled {
			allDestroyEnabled = false
			break
		}
	}

	if allDestroyEnabled {
		jobLog.Debug("skipping reconciliation for stack with destroy enabled",
			slog.String("event", action),
			slog.String("stack", stackName),
		)

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

	if shouldIgnoreRestartReconciliationForScheduledJob(action, event.Actor.Attributes) {
		jobLog.Debug("skipping reconciliation for scheduled restart-mode job completion",
			slog.String("event", action),
			slog.String("stack", stackName),
			slog.String("container_name", event.Actor.Attributes["name"]),
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
	if j.swarmModeForContext(contextName) {
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

	if contextName != "" {
		eventLog = eventLog.With(slog.String("context", contextName))
	}

	contextCLI := j.cliForContext(contextName)
	contextSwarmMode := j.swarmModeForContext(contextName)

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

		restartResult := j.restartContainer(ctx, eventLog, event, restartDC, contextCLI, contextSwarmMode)
		if restartResult.fallbackToDeploy {
			if event.Actor.ID != "" {
				waitForContainerRemovalSettled(ctx, eventLog, contextCLI.Client(), event.Actor.ID, containerRemovalSettleTimeout)
			}

			j.deploy(ctx, eventLog, stackDCs, action, event, traceID, contextName)
		}

		return
	}

	// When the event references a container that is being force-removed, Docker may
	// still report it as "Removing" by the time we begin reconciliation, which causes
	// docker compose to fail with "container is marked for removal and cannot be
	// started". Wait briefly for the container to either be fully removed or settle
	// into a stable state before re-deploying.
	if event.Actor.ID != "" {
		waitForContainerRemovalSettled(ctx, eventLog, contextCLI.Client(), event.Actor.ID, containerRemovalSettleTimeout)
	}

	j.deploy(ctx, eventLog, stackDCs, action, event, traceID, contextName)
}

func (j *job) deploy(ctx context.Context, jobLog *slog.Logger, dcs []*deployConfig.Config, action string, event events.Message, traceID string, contextName string) {
	repoLock := lock.GetRepoLock(j.info.metadata.Repository)
	repoLock.Lock()
	defer repoLock.Unlock()

	jobLog.Info("reconciliation started")
	defer jobLog.Info("reconciliation completed")

	// Use the context-specific CLI and only the deploy configs targeting this context
	// for cleanup, so we inspect the correct remote daemon for obsolete containers.
	contextCLI := j.cliForContext(contextName)
	contextSwarmMode := j.swarmModeForContext(contextName)
	contextDCs := j.deployConfigsForContext(contextName)

	if err := cleanupObsoleteAutoDiscoveredContainers(ctx, jobLog,
		contextCLI, contextSwarmMode, j.info.repoData.SourceUrl,
		contextDCs,
		j.info.metadata); err != nil {
		jobLog.Error("failed to clean up obsolete auto-discovered containers", logger.ErrAttr(err))
	}

	// Reconciliation deploys should always force recreate so missing containers are restored
	// even when there are no Git/compose changes.
	reconcileDCs := cloneDeployConfigsWithForcedRecreate(dcs)

	// Enrich metadata with reconciliation event information for deploy notifications
	actorKind := "container"
	if contextSwarmMode {
		actorKind = "service"
	}

	metadata := j.info.metadata
	metadata.ReconciliationEvent = action
	metadata.TraceID = strings.TrimSpace(traceID)
	metadata.AffectedActorKind = actorKind
	metadata.AffectedActorID = shortID(event.Actor.ID)
	metadata.AffectedActorName = strings.TrimSpace(event.Actor.Attributes["name"])

	// handleDeploy accepts the base CLI; it handles per-context routing internally.
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

	attributes := make(map[string]string, len(event.Actor.Attributes)+1)

	maps.Copy(attributes, event.Actor.Attributes)

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
