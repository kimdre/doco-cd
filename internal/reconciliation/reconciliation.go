package reconciliation

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

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

	filterArgs := make(client.Filters)
	filterArgs.Add("type", "container")
	filterArgs.Add("label", docker.DocoCDLabels.Metadata.Manager+"="+config.AppName)

	repositoryLabelValue := normalizeRepositoryLabelFromCloneURL(j.info.repoData.CloneURL)
	if j.info.payload != nil && strings.TrimSpace(j.info.payload.FullName) != "" {
		repositoryLabelValue = j.info.payload.FullName
	}

	filterArgs.Add("label", docker.DocoCDLabels.Repository.Name+"="+repositoryLabelValue)

	listenerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	eventResult := j.info.dockerCli.Client().Events(listenerCtx, client.EventsListOptions{
		Filters: filterArgs,
	})

	eventCh := eventResult.Messages
	errCh := eventResult.Err

	for {
		select {
		case <-ctx.Done():
			jobLog.Debug("ctx is done")
			return
		case <-j.closeChan:
			jobLog.Debug("channel is closed")
			return
		case err, ok := <-errCh:
			if !ok {
				jobLog.Debug("docker events error channel closed")
				return
			}

			if err != nil && !errors.Is(err, context.Canceled) {
				jobLog.Error("docker event listener failed", logger.ErrAttr(err))
			}
		case event, ok := <-eventCh:
			if !ok {
				jobLog.Debug("docker events channel closed")
				return
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

func (j *job) handleEvent(ctx context.Context, jobLog *slog.Logger, event events.Message) {
	action := normalizeReconciliationEventAction(string(event.Action))

	dcs := j.deployConfigGroupByEvent[action]
	if len(dcs) == 0 {
		return
	}

	eventLog := jobLog.With(
		slog.Group("trigger", slog.String("event", action)),
		slog.String("stack", event.Actor.Attributes[docker.DocoCDLabels.Deployment.Name]),
	)

	j.deploy(ctx, eventLog, dcs)
}

func (j *job) deploy(ctx context.Context, jobLog *slog.Logger, dcs []*config.DeployConfig) {
	repoLock := lock.GetRepoLock(j.info.metadata.Repository)
	repoLock.Lock()
	defer repoLock.Unlock()

	jobLog = jobLog.With(slog.String("reconciliation_id", id.GenID()))

	jobLog.Debug("reconciliation started")
	defer jobLog.Debug("reconciliation completed")

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
	if swarm.GetModeEnabled() {
		info.jobLog.Debug("skipping reconciliation event listener in swarm mode")
		return
	}

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

	if strings.HasPrefix(action, "health_status:") {
		parts := strings.SplitN(action, ":", 2)
		if len(parts) == 2 {
			action = strings.TrimSpace(parts[0]) + ": " + strings.TrimSpace(parts[1])
			if action == "health_status: unhealthy" {
				return "unhealthy"
			}
		}
	}

	if action == "unhealthy" {
		return action
	}

	return action
}
