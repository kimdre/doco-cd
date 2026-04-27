package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

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
	info jobInfo

	// key is the docker event action name (for example "die" or "unhealthy").
	deployConfigGroupByEvent map[string][]*config.DeployConfig
	closeChan                chan struct{}
}

func newJob(info jobInfo, deployConfigGroupByEvent map[string][]*config.DeployConfig) *job {
	return &job{
		info:                     info,
		deployConfigGroupByEvent: deployConfigGroupByEvent,
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

	jobLog.Debug("reconciliation loop started")
	defer jobLog.Debug("reconciliation loop stopped")

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
	repoName := gitInternal.GetRepoName(string(cloneURL))
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
	filters := make(map[string]struct{}, len(actions))

	for _, rawAction := range actions {
		action := normalizeReconciliationEventAction(rawAction)
		if action == "" {
			continue
		}

		switch action {
		case "unhealthy":
			// Only subscribe to unhealthy health transitions, not healthy ones.
			filters["health_status: unhealthy"] = struct{}{}
		case "destroy":
			if swarmMode {
				filters["remove"] = struct{}{}
				continue
			}

			filters["destroy"] = struct{}{}
		default:
			filters[action] = struct{}{}
		}
	}

	return mapsKeys(filters)
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
		j.restartContainer(ctx, eventLog, event, stackDCs)
		return
	}

	j.deploy(ctx, eventLog, stackDCs)
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

func stackNameFromEvent(event events.Message, candidates []*config.DeployConfig) string {
	attrs := event.Actor.Attributes

	for _, key := range []string{docker.DocoCDLabels.Deployment.Name, swarm.StackNamespaceLabel} {
		v := strings.TrimSpace(attrs[key])
		if v != "" {
			return v
		}
	}

	serviceName := strings.TrimSpace(attrs["name"])
	if serviceName == "" {
		serviceName = strings.TrimSpace(attrs["service"])
	}

	if serviceName == "" {
		return ""
	}

	for _, dc := range candidates {
		if serviceName == dc.Name || strings.HasPrefix(serviceName, dc.Name+"_") {
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

	if err := handleDeploy(ctx, jobLog, j.info.appConfig,
		j.info.dataMountPoint, j.info.dockerCli,
		j.info.secretProvider, j.info.metadata.JobID, j.info.jobTrigger,
		j.info.repoData, dcs, j.info.payload, j.info.testName); err != nil {
		jobLog.Error("failed to deploy", logger.ErrAttr(err))
	}
}

// restartContainer restarts a single container identified by the Docker event,
// using the restart timeout configured in the deploy config (default 10 s).
// Used for restart-oriented events where the container is still present.
func (j *job) restartContainer(ctx context.Context, jobLog *slog.Logger, event events.Message, dcs []*config.DeployConfig) {
	containerID := event.Actor.ID
	containerName := event.Actor.Attributes["name"]

	timeout := 10 // seconds — safe default
	if len(dcs) > 0 && dcs[0].Reconciliation.RestartTimeout > 0 {
		timeout = dcs[0].Reconciliation.RestartTimeout
	}

	restartLog := jobLog.With(
		slog.String("container_id", containerID),
		slog.String("container_name", containerName),
		slog.Int("restart_timeout", timeout),
	)

	restartLog.Info("restarting container")

	metadata := notification.Metadata{
		Repository: j.info.metadata.Repository,
		Stack:      j.info.metadata.Stack,
		JobID:      j.info.metadata.JobID,
	}

	if _, err := j.info.dockerCli.Client().ContainerRestart(ctx, containerID, client.ContainerRestartOptions{
		Timeout: &timeout,
	}); err != nil {
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

	repoJobs map[string]*job
}

func newReconciliation() *reconciliation {
	return &reconciliation{
		repoJobs: make(map[string]*job),
		m:        sync.Mutex{},
	}
}

func (r *reconciliation) close() {
	r.m.Lock()
	defer r.m.Unlock()

	for _, job := range r.repoJobs {
		job.close()
	}

	r.repoJobs = make(map[string]*job)
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

func mapsKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))

	for key := range m {
		keys = append(keys, key)
	}

	return keys
}
