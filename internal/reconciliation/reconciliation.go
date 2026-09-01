package reconciliation

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/id"

	"github.com/kimdre/doco-cd/internal/config/app"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"

	"github.com/kimdre/doco-cd/internal/docker"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/logger"
)

const reconciliationTraceIDAttr = "doco_cd_reconciliation_trace_id"

// Rewind the Docker events since-cursor slightly to avoid precision edge cases
// around listener restarts and startup recovery.
const reconciliationSinceSafetySkew = 3 * time.Second

// contextualEvent pairs a Docker daemon event with the context name it originated from.
type contextualEvent struct {
	event       events.Message
	contextName string
	swarmMode   bool
}

// initContextCLIs populates j.contextCLIs with a Docker CLI entry for the default context
// and for every unique non-default context referenced in the job's deploy configs.
func (j *job) initContextCLIs(ctx context.Context) {
	contextCLIs := make(map[string]contextCLIEntry)

	for _, dc := range j.info.DeployConfigs {
		ctxName := docker.NormalizeContextName(dc.Context)
		if _, already := contextCLIs[ctxName]; already {
			continue
		}

		entry := resolveDeployContext(ctx, j.manager.contexts, ctxName)
		if entry.err != nil {
			j.info.Logger.Error("failed to create Docker CLI for context; skipping event listener for that context",
				slog.String("context", docker.DisplayContextName(ctxName)),
				logger.ErrAttr(entry.err),
			)

			continue
		}

		contextCLIs[ctxName] = contextCLIEntry{
			cli:       entry.cli,
			swarmMode: entry.swarmMode,
		}
	}

	j.contextCLIs = contextCLIs
}

// cliForContext returns the Docker CLI for the given context name, falling back to the
// default CLI if the context is not found.
func (j *job) cliForContext(contextName string) command.Cli {
	contextName = docker.NormalizeContextName(contextName)
	if j.contextCLIs != nil {
		if e, ok := j.contextCLIs[contextName]; ok {
			return e.cli
		}
	}

	return j.manager.dockerCli
}

// swarmModeForContext returns the capability resolved when this job initialized
// the requested Docker context.
func (j *job) swarmModeForContext(contextName string) bool {
	contextName = docker.NormalizeContextName(contextName)
	if j.contextCLIs != nil {
		if e, ok := j.contextCLIs[contextName]; ok {
			return e.swarmMode
		}
	}

	return false
}

// deployConfigsForContext returns the subset of the job's deploy configs that target contextName.
func (j *job) deployConfigsForContext(contextName string) []*deployConfig.Config {
	return filterConfigsByContext(j.info.DeployConfigs, contextName)
}

func (j *job) deployConfigsForContextMode(contextName string, swarmMode bool) []*deployConfig.Config {
	return filterConfigsByMode(j.deployConfigsForContext(contextName), j.swarmModeForContext(contextName), swarmMode)
}

func (j *job) run(ctx context.Context) {
	jobLog := j.info.Logger

	j.initContextCLIs(ctx)

	// Wait for all event-listener goroutines to exit before closing their Docker
	// CLIs, so we never close a client that a listener is still using.
	var listenerWG sync.WaitGroup

	defer listenerWG.Wait()

	// Startup recovery: run for every configured context in parallel.
	// Run both checks concurrently per context, then wait for all to finish
	// before subscribing to Docker events so startup healing happens against
	// a stable initial view of the daemon state.
	var startupRecoveryWG sync.WaitGroup

	for ctxName, entry := range j.contextCLIs {
		for swarmMode, configs := range groupDeployConfigsByMode(j.deployConfigsForContext(ctxName), entry.swarmMode) {
			if len(configs) == 0 {
				continue
			}

			unhealthyConfigs := filterConfigsByMode(
				getDeployConfigGroupByEvent(configs)["unhealthy"],
				entry.swarmMode,
				swarmMode,
			)

			startupRecoveryWG.Add(2)

			go func(entry contextCLIEntry, swarmMode bool, unhealthyConfigs []*deployConfig.Config) {
				defer startupRecoveryWG.Done()

				j.restartUnhealthyContainersOnStartup(ctx, jobLog, entry.cli, swarmMode, unhealthyConfigs)
			}(entry, swarmMode, unhealthyConfigs)

			go func(ctxName string, entry contextCLIEntry, swarmMode bool, configs []*deployConfig.Config) {
				defer startupRecoveryWG.Done()

				j.redeployMissingServicesOnStartup(ctx, jobLog, ctxName, entry.cli, swarmMode, configs)
			}(ctxName, entry, swarmMode, configs)
		}
	}

	startupRecoveryWG.Wait()

	// Fan-in Docker events from all contexts into a single channel processed serially.
	// The buffer absorbs short bursts from multiple daemons without backpressure.
	mergedCh := make(chan contextualEvent, 256)
	listenerReadyCh := make(chan struct{}, len(j.contextCLIs))
	expectedListeners := 0

	for ctxName, entry := range j.contextCLIs {
		for swarmMode, configs := range groupDeployConfigsByMode(j.deployConfigsForContext(ctxName), entry.swarmMode) {
			if len(configs) == 0 {
				continue
			}

			expectedListeners++

			listenerWG.Add(1)

			go func(ctxName string, entry contextCLIEntry, swarmMode bool, configs []*deployConfig.Config) {
				defer listenerWG.Done()

				j.runContextEventListener(ctx, jobLog, ctxName, entry, swarmMode, configs, mergedCh, listenerReadyCh)
			}(ctxName, entry, swarmMode, configs)
		}
	}

	if expectedListeners == 0 {
		j.signalReady()
	} else {
		go func() {
			ready := 0
			for ready < expectedListeners {
				select {
				case <-ctx.Done():
					return
				case <-j.closeChan:
					return
				case <-listenerReadyCh:
					ready++
				}
			}

			j.signalReady()
		}()
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

			j.handleEvent(ctx, jobLog, ce.event, ce.contextName, ce.swarmMode)
		}
	}
}

// runContextEventListener connects to the Docker daemon for entry, listens for relevant events,
// forwards them (tagged with contextName) to out, and automatically reconnects on disconnection.
func (j *job) runContextEventListener(ctx context.Context, jobLog *slog.Logger, contextName string, entry contextCLIEntry, swarmMode bool, contextDCs []*deployConfig.Config, out chan<- contextualEvent, ready chan<- struct{}) {
	repositoryLabelValue := gitInternal.GetFullName(j.info.Repository.SourceUrl)
	if j.info.Payload != nil && strings.TrimSpace(j.info.Payload.FullName) != "" {
		repositoryLabelValue = j.info.Payload.FullName
	}

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
	readySignaled := false

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

		if !readySignaled {
			readySignaled = true

			ready <- struct{}{}
		}

		reconnect, newestEventTime := j.forwardEvents(ctx, jobLog, eventResult.Messages, eventResult.Err, contextName, swarmMode, out)

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
func (j *job) forwardEvents(ctx context.Context, jobLog *slog.Logger, eventCh <-chan events.Message, errCh <-chan error, contextName string, swarmMode bool, out chan<- contextualEvent) (bool, time.Time) {
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
				if isFatalConnectionError(err) {
					jobLog.Error("docker event listener stopped: unrecoverable connection error", slog.String("context", contextName), logger.ErrAttr(err))

					return false, newestEventTime // do not retry
				}

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
			case out <- contextualEvent{event: event, contextName: contextName, swarmMode: swarmMode}:
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

func (j *job) handleEvent(ctx context.Context, jobLog *slog.Logger, event events.Message, contextName string, swarmMode bool) {
	action := normalizeReconciliationEventAction(string(event.Action))
	// Restrict candidates to the context the event originated from, since stack
	// names are only guaranteed to be unique within a single Docker context.
	dcs := filterConfigsByMode(filterConfigsByContext(j.deployConfigGroupByEvent[action], contextName), j.swarmModeForContext(contextName), swarmMode)

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

	if j.manager.deployments.isInProgress(j.info.Metadata.Repository, contextName, stackName) {
		jobLog.Debug("suppressing reconciliation event while stack deployment is in progress",
			slog.String("event", action),
			slog.String("stack", stackName),
		)

		return
	}

	// Scheduler stop holds are only registered for Compose-mode jobs, keyed by
	// project/service. A Swarm service on the same context can carry matching
	// labels, so restricting the check to Compose events avoids suppressing an
	// unrelated Swarm event.
	if !swarmMode && j.manager.schedulerHolds.isHeld(contextName, event.Actor.Attributes) {
		jobLog.Debug("suppressing reconciliation event for service intentionally held stopped by job scheduler",
			slog.String("event", action),
			slog.String("stack", stackName),
			slog.String("container_name", event.Actor.Attributes["name"]),
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

	stackID := j.info.Metadata.Repository + "/" + contextName + "/" + stackName
	stackLock := lock.GetRepoLock(stackID)

	if !stackLock.TryLock(id.New()) {
		jobLog.Debug("skipping reconciliation, already in progress for this stack", slog.String("stack", stackName))
		return
	}
	defer stackLock.Unlock()

	actorGroupName := "container"
	if swarmMode {
		actorGroupName = "service"
	}

	traceID := id.New()
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
	// For restart-oriented events the container is still present, so restart it
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

		restartResult := j.restartContainer(ctx, eventLog, event, restartDC, contextCLI, swarmMode)
		if restartResult.fallbackToDeploy {
			if event.Actor.ID != "" {
				j.waitForContainerRemovalSettled(ctx, eventLog, contextCLI.Client(), event.Actor.ID, containerRemovalSettleTimeout)
			}

			j.deploy(ctx, eventLog, stackDCs, action, event, traceID, contextName, swarmMode)
		}

		return
	}

	// When the event references a container that is being force-removed, Docker may
	// still report it as "Removing" by the time we begin reconciliation, which causes
	// docker compose to fail with "container is marked for removal and cannot be
	// started". Wait briefly for the container to either be fully removed or settle
	// into a stable state before re-deploying.
	if event.Actor.ID != "" {
		j.waitForContainerRemovalSettled(ctx, eventLog, contextCLI.Client(), event.Actor.ID, containerRemovalSettleTimeout)
	}

	j.deploy(ctx, eventLog, stackDCs, action, event, traceID, contextName, swarmMode)
}

func (j *job) deploy(ctx context.Context, jobLog *slog.Logger, dcs []*deployConfig.Config, action string, event events.Message, traceID string, contextName string, swarmMode bool) {
	repoLock := lock.GetRepoLock(j.info.Metadata.Repository)
	if !repoLock.LockContext(ctx, traceID) {
		jobLog.Debug("reconciliation skipped, context cancelled while waiting for repository lock")

		return
	}

	defer repoLock.Unlock()

	jobLog.Info("reconciliation started")
	defer jobLog.Info("reconciliation completed")

	// Use the context-specific CLI and only the deploy configs targeting this context
	// for cleanup, so we inspect the correct remote daemon for obsolete containers.
	contextCLI := j.cliForContext(contextName)
	contextDCs := j.deployConfigsForContextMode(contextName, swarmMode)

	if err := cleanupObsoleteAutoDiscoveredContainers(ctx, jobLog,
		contextCLI, swarmMode, contextName, j.info.Repository.SourceUrl,
		contextDCs,
		j.info.Metadata, j.manager.notifier); err != nil {
		jobLog.Error("failed to clean up obsolete auto-discovered containers", logger.ErrAttr(err))
	}

	// Reconciliation deploys should always force recreate so missing containers are restored
	// even when there are no Git/compose changes.
	reconcileDCs := cloneDeployConfigsWithForcedRecreate(dcs)

	// Enrich metadata with reconciliation event information for deploy notifications
	actorKind := "container"
	if swarmMode {
		actorKind = "service"
	}

	metadata := j.info.Metadata
	metadata.ReconciliationEvent = action
	metadata.TraceID = strings.TrimSpace(traceID)
	metadata.AffectedActorKind = actorKind
	metadata.AffectedActorID = shortID(event.Actor.ID)
	metadata.AffectedActorName = strings.TrimSpace(event.Actor.Attributes["name"])

	// handleDeploy accepts the base CLI; it handles per-context routing internally.
	req := j.info
	req.Metadata = metadata
	req.DeployConfigs = reconcileDCs

	if err := j.manager.handleDeploy(ctx, req); err != nil {
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

// isFatalConnectionError reports whether err represents a permanent, non-retryable
// connection failure (e.g. a required transport binary like "ssh" is missing from PATH).
// These errors will not resolve on their own, so the event listener should stop instead
// of retrying indefinitely.
func isFatalConnectionError(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()

	fatalSubstrings := []string{
		"executable file not found in $PATH",
		"no such file or directory",
	}

	return slices.ContainsFunc(fatalSubstrings, func(s string) bool {
		return strings.Contains(msg, s)
	})
}
