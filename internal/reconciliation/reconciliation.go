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

	// key is the interval in second
	deployConfigGroupByInterval map[int][]*config.DeployConfig
	closeChan                   chan struct{}
}

func newJob(info jobInfo, deployConfigGroupByInterval map[int][]*config.DeployConfig) *job {
	return &job{
		info:                        info,
		deployConfigGroupByInterval: deployConfigGroupByInterval,
		closeChan:                   make(chan struct{}),
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

	wg := sync.WaitGroup{}

	for interval, dcs := range j.deployConfigGroupByInterval {
		if len(dcs) > 0 {
			wg.Go(func() {
				j.runByInterval(ctx, interval, dcs)
			})
		}
	}

	wg.Wait()
}

func (j *job) runByInterval(ctx context.Context, interval int, dcs []*config.DeployConfig) {
	if len(dcs) == 0 {
		return
	}

	jobLog := j.info.jobLog.With(
		slog.Int("interval", interval),
	)

	jobLog.Debug("reconciliation interval worker started")
	defer jobLog.Debug("reconciliation interval worker stopped")

	for {
		select {
		case <-ctx.Done():
			jobLog.Debug("ctx is done")
			return
		case <-j.closeChan:
			jobLog.Debug("channel is closed")
			return
		case <-time.After(time.Second * time.Duration(interval)):
			j.deploy(ctx, jobLog, dcs)
		}
	}
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
	cfg := getDeployConfigGroupByInterval(info.deployConfigs)
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

func getDeployConfigGroupByInterval(dcs []*config.DeployConfig) map[int][]*config.DeployConfig {
	m := make(map[int][]*config.DeployConfig)

	for _, dc := range dcs {
		if r := dc.Reconciliation; r.Enabled {
			m[r.Interval] = append(m[r.Interval], dc)
		}
	}

	return m
}

func init() {
	graceful.RegistryShutdownFunc(func() {
		reconciliationHandler.close()
	})
}
