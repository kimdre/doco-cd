package reconciliation

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

var reconciliationHandler *reconciliation

func init() {
	reconciliationHandler = &reconciliation{
		repoJobs: make(map[string]*job),
		m:        sync.Mutex{},
	}
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
	info     jobInfo
	Interval int // second

	closeChan chan struct{}
}

func newJob(info jobInfo, interval int) *job {
	return &job{
		info:      info,
		Interval:  interval,
		closeChan: make(chan struct{}),
	}
}

func (j *job) close() {
	close(j.closeChan)
}

func (j *job) run(ctx context.Context) {
	for {
		jobLog := j.info.jobLog
		select {
		case <-ctx.Done():
			jobLog.Debug("ctx.Done, closing job reconciliation")
			return
		case <-j.closeChan:
			jobLog.Debug("closed, closing job reconciliation")
			return
		case <-time.After(time.Second * time.Duration(j.Interval)):
			jobLog.Debug("time to run reconciliation")
			j.deploy(ctx)
		}
	}
}

func (j *job) deploy(ctx context.Context) {
	repoLock := lock.GetRepoLock(j.info.metadata.Repository)
	repoLock.Lock()
	defer repoLock.Unlock()

	err := deploy(ctx, j.info.jobLog, j.info.appConfig,
		j.info.dataMountPoint, j.info.dockerCli, j.info.secretProvider,
		j.info.metadata, j.info.jobTrigger,
		j.info.repoData, j.info.deployConfigs,
		j.info.payload, j.info.testName)
	if err != nil {
		slog.Error("failed to deploy", logger.ErrAttr(err))
	}
}

type reconciliation struct {
	m sync.Mutex

	repoJobs map[string]*job
}

func (r *reconciliation) addJob(ctx context.Context, info jobInfo) {
	cfg, interval := getDeployConfig(info.deployConfigs)
	if len(cfg) == 0 {
		return
	}

	info.deployConfigs = cfg

	r.m.Lock()
	defer r.m.Unlock()

	old := r.repoJobs[info.repoData.Name]
	old.close()

	// start new
	newJob := newJob(info, interval)

	r.repoJobs[info.repoData.Name] = newJob
	go newJob.run(context.WithoutCancel(ctx))
}

func getDeployConfig(deployConfigs []*config.DeployConfig) ([]*config.DeployConfig, int) {
	enabled := []*config.DeployConfig{}
	// todo different interval
	// auto delete need to be considered
	minInterval := math.MaxInt

	for _, deployConfig := range deployConfigs {
		if r := deployConfig.Reconciliation; r.Enabled {
			enabled = append(enabled, deployConfig)

			if r.Interval < minInterval {
				minInterval = r.Interval
			}
		}
	}

	return enabled, minInterval
}
