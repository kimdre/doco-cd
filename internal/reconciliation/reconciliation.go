package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/graceful"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/utils/id"
	"github.com/kimdre/doco-cd/internal/utils/set"
	"github.com/kimdre/doco-cd/internal/webhook"
)

var reconciliationHandler *reconciliation

func init() {
	reconciliationHandler = newReconciliation()
}

type jobInfo struct {
	appConfig      *config.AppConfig
	dataMountPoint container.MountPoint
	dockerCli      command.Cli
	secretProvider *secretprovider.SecretProvider

	jobLog *slog.Logger

	metadata      notification.Metadata
	jobTrigger    stages.JobTrigger
	repoData      stages.RepositoryData
	deployConfigs []*config.DeployConfig
	payload       *webhook.ParsedPayload
	testName      string
}

type job struct {
	info                     jobInfo
	deployConfigGroupByEvent map[string][]*config.DeployConfig // key is the docker event action name (for example "die" or "unhealthy").
	unhealthyRestartHistory  map[string][]time.Time            // key is the docker container ID, value is the list of timestamps of recent unhealthy restart events for that container.
	restartSuppressUntil     map[string]time.Time              // key is the docker container ID that was restarted, value is the timestamp until which follow-up events from that restart should be suppressed.
	closeChan                chan struct{}
}

func init() {
	graceful.RegistryShutdownFunc("close_reconciliation", func() {
		reconciliationHandler.close()
	})
}

func newJob(info jobInfo, deployConfigGroupByEvent map[string][]*config.DeployConfig) *job {
	return &job{
		info:                     info,
		deployConfigGroupByEvent: deployConfigGroupByEvent,
		unhealthyRestartHistory:  make(map[string][]time.Time),
		restartSuppressUntil:     make(map[string]time.Time),
		closeChan:                make(chan struct{}),
	}
}

func (j *job) close() {
	if j == nil {
		return
	}

	close(j.closeChan)
}

func (j *job) run(ctx context.Context) {
	jobLog := j.info.jobLog

	swarmMode := swarm.GetModeEnabled()

	filterArgs := make(client.Filters)
	filterArgs.Add("type", dockerEventTypeForMode(swarmMode))

	if !swarmMode {
		filterArgs.Add("label", docker.DocoCDLabels.Metadata.Manager+"="+config.AppName)

		repositoryLabelValue := normalizeRepositoryLabelFromCloneURL(j.info.repoData.CloneURL)
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
	j.restartUnhealthyContainersOnStartup(ctx, jobLog)

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

// normalizeRepositoryLabelFromCloneURL mirrors the repository-name normalization used during poll deployment labeling.
func normalizeRepositoryLabelFromCloneURL(cloneURL config.HttpUrl) string {
	repoName := gitInternal.GetFullName(string(cloneURL))
	parts := strings.Split(repoName, "/")

	if len(parts) > 2 {
		return strings.Join(parts[1:], "/")
	}

	if len(parts) == 2 {
		return parts[1]
	}

	return repoName
}

func dockerEventTypeForMode(swarmMode bool) string {
	if swarmMode {
		return "service"
	}

	return "container"
}

func dockerEventFiltersForActions(actions []string, swarmMode bool) []string {
	filters := set.New[string]()

	for _, rawAction := range actions {
		action := normalizeReconciliationEventAction(rawAction)
		if action == "" {
			continue
		}

		switch action {
		case "unhealthy":
			// Only subscribe to unhealthy health transitions, not healthy ones.
			filters.Add("health_status: unhealthy")
		case "destroy":
			if swarmMode {
				filters.Add("remove")
				continue
			}

			filters.Add("destroy")
		default:
			filters.Add(action)
		}
	}

	return filters.ToSlice()
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
			slog.String("container_id", event.Actor.ID),
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

	eventLog := logger.
		WithoutAttr(jobLog, "job_id").
		With(
			slog.Group("reconciliation",
				slog.String("event", action),
				slog.String("trace_id", id.GenID()),
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

	j.deploy(ctx, eventLog, stackDCs)
}

// containerRemovalSettleTimeout caps how long handleEvent waits for a force-removed
// container to be fully gone before kicking off a reconciliation deploy.
const containerRemovalSettleTimeout = 15 * time.Second

// waitForContainerRemovalSettled polls the given container until it is either gone
// (inspect returns not-found) or no longer reported as "removing", or until the
// timeout elapses. This prevents a race between Docker's async container teardown
// and docker compose trying to recreate the container.
func waitForContainerRemovalSettled(ctx context.Context, jobLog *slog.Logger, cli client.APIClient, containerID string, timeout time.Duration) {
	if containerID == "" || timeout <= 0 {
		return
	}

	deadline := time.Now().Add(timeout)

	for {
		inspectResult, err := cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
		if err != nil {
			// Treat any inspect error (most importantly "no such container") as
			// "container is gone, safe to proceed".
			if errdefs.IsNotFound(err) {
				return
			}

			jobLog.Debug("failed to inspect container while waiting for removal to settle",
				slog.String("container_id", containerID),
				logger.ErrAttr(err),
			)

			return
		}

		state := inspectResult.Container.State
		if state == nil || !strings.EqualFold(strings.TrimSpace(string(state.Status)), "removing") {
			return
		}

		if !time.Now().Before(deadline) {
			jobLog.Debug("timed out waiting for container removal to settle",
				slog.String("container_id", containerID),
				slog.Duration("timeout", timeout),
			)

			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func deployConfigsByName(dcs []*config.DeployConfig, name string) []*config.DeployConfig {
	result := make([]*config.DeployConfig, 0, len(dcs))

	for _, dc := range dcs {
		if dc.Name == name {
			result = append(result, dc)
		}
	}

	return result
}

// stackNameFromEvent attempts to determine the stack name referenced by the given Docker event
// by examining various event attributes and matching them against the candidate config.DeployConfig configs.
// Returns an empty string when no stack name could be determined or matched.
func stackNameFromEvent(event events.Message, candidates []*config.DeployConfig) string {
	attrs := event.Actor.Attributes

	for _, key := range []string{docker.DocoCDLabels.Deployment.Name, swarm.StackNamespaceLabel} {
		v := strings.TrimSpace(attrs[key])
		if v != "" {
			if matched := matchCandidateStackName(v, candidates); matched != "" {
				return matched
			}
		}
	}

	for _, key := range []string{"name", "service", "com.docker.swarm.service.name", "com.docker.swarm.task.name"} {
		identifier := strings.TrimSpace(attrs[key])
		if identifier == "" {
			continue
		}

		if matched := matchCandidateStackName(identifier, candidates); matched != "" {
			return matched
		}
	}

	return ""
}

// matchCandidateStackName checks if the given identifier matches any of the candidate DeployConfig stack names,
// either as an exact match or as a prefix followed by typical Docker naming separators.
func matchCandidateStackName(identifier string, candidates []*config.DeployConfig) string {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return ""
	}

	for _, dc := range candidates {
		if dc == nil {
			continue
		}

		name := strings.TrimSpace(dc.Name)
		if name == "" {
			continue
		}

		if identifier == name {
			return name
		}

		// Docker Swarm service names are typically formatted as <stack>_<service>.
		// Some event attributes can also contain task/container names such as
		// <stack>_<service>.<slot>.<id>, so matching by prefix keeps this resilient.
		if strings.HasPrefix(identifier, name+"_") ||
			strings.HasPrefix(identifier, name+".") ||
			strings.HasPrefix(identifier, name+"-") {
			return dc.Name
		}
	}

	return ""
}

func (j *job) deploy(ctx context.Context, jobLog *slog.Logger, dcs []*config.DeployConfig) {
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

	if err := handleDeploy(ctx, jobLog, j.info.appConfig,
		j.info.dataMountPoint, j.info.dockerCli,
		j.info.secretProvider, j.info.metadata.JobID, j.info.jobTrigger,
		j.info.repoData, reconcileDCs, j.info.payload, j.info.testName); err != nil {
		jobLog.Error("failed to deploy", logger.ErrAttr(err))
	}
}

func cloneDeployConfigsWithForcedRecreate(dcs []*config.DeployConfig) []*config.DeployConfig {
	reconcileDCs := make([]*config.DeployConfig, len(dcs))

	for i, dc := range dcs {
		dcCopy := *dc
		dcCopy.ForceRecreate = true
		reconcileDCs[i] = &dcCopy
	}

	return reconcileDCs
}

// restartContainer restarts a single container identified by the Docker event,
// using the restart timeout configured in the deploy config (default 10 s).
// Used for restart-oriented events where the container is still present.
func (j *job) restartContainer(ctx context.Context, jobLog *slog.Logger, event events.Message, dc *config.DeployConfig) {
	containerID := event.Actor.ID
	containerName := event.Actor.Attributes["name"]
	restartOpts := restartOptionsFromDeployConfig(dc)

	restartTimeout := 10
	if restartOpts.Timeout != nil {
		restartTimeout = *restartOpts.Timeout
	}

	restartLog := jobLog.With(
		slog.String("container_id", containerID),
		slog.String("container_name", containerName),
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
			"Container restart failed",
			fmt.Sprintf("container %s (%s) could not be restarted: %s", containerName, containerID, err.Error()),
			metadata,
		); notifyErr != nil {
			restartLog.Error("failed to send restart failure notification", logger.ErrAttr(notifyErr))
		}

		return
	}

	restartLog.Info("container restarted successfully")

	if notifyErr := notification.Send(
		notification.Success,
		"Container restarted",
		fmt.Sprintf("container %s (%s) was restarted successfully", containerName, containerID),
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

func (j *job) restartUnhealthyContainersOnStartup(ctx context.Context, jobLog *slog.Logger) {
	unhealthyDCs := j.deployConfigGroupByEvent["unhealthy"]
	if len(unhealthyDCs) == 0 || swarm.GetModeEnabled() {
		return
	}

	repositoryLabelValue := normalizeRepositoryLabelFromCloneURL(j.info.repoData.CloneURL)
	if j.info.payload != nil && strings.TrimSpace(j.info.payload.FullName) != "" {
		repositoryLabelValue = j.info.payload.FullName
	}

	filterArgs := make(client.Filters)
	filterArgs.Add("label", docker.DocoCDLabels.Metadata.Manager+"="+config.AppName)
	filterArgs.Add("label", docker.DocoCDLabels.Repository.Name+"="+repositoryLabelValue)

	containerResult, err := j.info.dockerCli.Client().ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filterArgs})
	if err != nil {
		jobLog.Error("failed to list containers for startup unhealthy scan", logger.ErrAttr(err))
		return
	}

	for _, c := range containerResult.Items {
		stackName := strings.TrimSpace(c.Labels[docker.DocoCDLabels.Deployment.Name])
		if stackName == "" {
			continue
		}

		stackDCs := deployConfigsByName(unhealthyDCs, stackName)

		restartDC := selectRestartDeployConfig(stackDCs, c.Labels)
		if restartDC == nil {
			continue
		}

		inspectResult, err := j.info.dockerCli.Client().ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			jobLog.Debug("failed to inspect container during startup unhealthy scan",
				slog.String("container_id", c.ID),
				logger.ErrAttr(err),
			)

			continue
		}

		inspect := inspectResult.Container

		if inspect.State == nil || inspect.State.Health == nil || strings.ToLower(strings.TrimSpace(string(inspect.State.Health.Status))) != "unhealthy" {
			continue
		}

		containerName := ""
		if len(c.Names) > 0 {
			containerName = strings.TrimPrefix(c.Names[0], "/")
		}

		eventLog := logger.
			WithoutAttr(jobLog, "job_id").
			With(
				slog.Group("reconciliation",
					slog.String("event", "startup_unhealthy"),
					slog.String("trace_id", id.GenID()),
				),
				slog.String("stack", stackName),
			)

		j.restartContainer(ctx, eventLog, events.Message{
			Action: events.Action("unhealthy"),
			Actor: events.Actor{
				ID: c.ID,
				Attributes: map[string]string{
					"name": containerName,
				},
			},
		}, restartDC)
	}
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

type reconciliation struct {
	m sync.Mutex

	repoJobs        map[string]*job
	deployingStacks map[string]int
}

func newReconciliation() *reconciliation {
	return &reconciliation{
		repoJobs:        make(map[string]*job),
		deployingStacks: make(map[string]int),
		m:               sync.Mutex{},
	}
}

func (r *reconciliation) close() {
	r.m.Lock()
	defer r.m.Unlock()

	for _, job := range r.repoJobs {
		job.close()
	}

	r.repoJobs = make(map[string]*job)
	r.deployingStacks = make(map[string]int)
}

func stackDeploymentKey(repository, stack string) string {
	return repository + "/" + stack
}

func (r *reconciliation) startStackDeployment(repository, stack string) {
	if repository == "" || stack == "" {
		return
	}

	key := stackDeploymentKey(repository, stack)

	r.m.Lock()
	r.deployingStacks[key]++
	r.m.Unlock()
}

func (r *reconciliation) finishStackDeployment(repository, stack string) {
	if repository == "" || stack == "" {
		return
	}

	key := stackDeploymentKey(repository, stack)

	r.m.Lock()
	defer r.m.Unlock()

	count := r.deployingStacks[key]
	if count <= 1 {
		delete(r.deployingStacks, key)
		return
	}

	r.deployingStacks[key] = count - 1
}

func (r *reconciliation) isStackDeploymentInProgress(repository, stack string) bool {
	if repository == "" || stack == "" {
		return false
	}

	key := stackDeploymentKey(repository, stack)

	r.m.Lock()
	defer r.m.Unlock()

	return r.deployingStacks[key] > 0
}

func (r *reconciliation) addJob(ctx context.Context, info jobInfo) {
	cfg := getDeployConfigGroupByEvent(info.deployConfigs)
	if len(cfg) == 0 {
		return
	}

	r.m.Lock()
	defer r.m.Unlock()

	old := r.repoJobs[info.repoData.Name]
	old.close()

	// start new
	newJob := newJob(info, cfg)

	r.repoJobs[info.repoData.Name] = newJob
	go newJob.run(context.WithoutCancel(ctx))
}

func getDeployConfigGroupByEvent(dcs []*config.DeployConfig) map[string][]*config.DeployConfig {
	m := make(map[string][]*config.DeployConfig)

	for _, dc := range dcs {
		if r := dc.Reconciliation; r.Enabled {
			for _, event := range r.Events {
				action := normalizeReconciliationEventAction(event)
				if action == "" {
					continue
				}

				m[action] = append(m[action], dc)
			}
		}
	}

	return m
}

func normalizeReconciliationEventAction(action string) string {
	action = strings.ToLower(strings.TrimSpace(action))
	action = strings.Join(strings.Fields(action), " ")

	switch action {
	case "remove", "delete":
		return "destroy"
	case "health_status: unhealthy":
		return "unhealthy"
	}

	return action
}

// mapsKeys returns the keys of the given map as a slice.
func mapsKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))

	for key := range m {
		keys = append(keys, key)
	}

	return keys
}
