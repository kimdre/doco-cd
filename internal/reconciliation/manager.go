package reconciliation

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/graceful"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

var reconciliationHandler *reconciliation

func init() {
	reconciliationHandler = newReconciliation()
}

func init() {
	graceful.RegistryShutdownFunc("close_reconciliation", func() {
		reconciliationHandler.close()
	})
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
