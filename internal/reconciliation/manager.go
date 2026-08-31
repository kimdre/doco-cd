package reconciliation

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/app"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"

	"github.com/kimdre/doco-cd/internal/docker"
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

// contextCLIEntry holds a Docker CLI and its resolved metadata for one Docker context.
type contextCLIEntry struct {
	cli       command.Cli
	closeFn   func() // nil for the default context (which is always j.info.dockerCli)
	swarmMode bool
}

type jobInfo struct {
	appConfig      *app.Config
	dataMountPoint container.MountPoint
	dockerCli      command.Cli
	contexts       *docker.ContextRegistry
	secretProvider secretprovider.SecretProvider

	jobLog *slog.Logger

	metadata      notification.Metadata
	jobTrigger    stages.JobTrigger
	repoData      stages.RepositoryData
	deployConfigs []*deployConfig.Config
	payload       *webhook.ParsedPayload
	testName      string
}

type job struct {
	info                     jobInfo
	deployConfigGroupByEvent map[string][]*deployConfig.Config // key is the docker event action name (for example "die" or "unhealthy").
	restartStateMu           sync.Mutex                        // guards unhealthyRestartHistory and restartSuppressUntil against concurrent access from parallel per-context startup recovery goroutines.
	unhealthyRestartHistory  map[string][]time.Time            // key is the docker container ID, value is the list of timestamps of recent unhealthy restart events for that container.
	restartSuppressUntil     map[string]time.Time              // key is the docker container ID that was restarted, value is the timestamp until which follow-up events from that restart should be suppressed.
	closeChan                chan struct{}
	readyChan                chan struct{}
	readyOnce                sync.Once
	// contextCLIs maps context name (empty string = default) to its Docker CLI and metadata.
	// Populated at the start of run() and closed when the job exits.
	contextCLIs map[string]contextCLIEntry
}

func newJob(info jobInfo, deployConfigGroupByEvent map[string][]*deployConfig.Config) *job {
	return &job{
		info:                     info,
		deployConfigGroupByEvent: deployConfigGroupByEvent,
		unhealthyRestartHistory:  make(map[string][]time.Time),
		restartSuppressUntil:     make(map[string]time.Time),
		closeChan:                make(chan struct{}),
		readyChan:                make(chan struct{}),
	}
}

func (j *job) close() {
	if j == nil {
		return
	}

	close(j.closeChan)
}

func (j *job) signalReady() {
	if j == nil {
		return
	}

	j.readyOnce.Do(func() {
		close(j.readyChan)
	})
}

// schedulerHoldEntry tracks how many concurrent scheduled jobs are holding a
// service stopped. When the last holder releases, the entry stays active for a
// grace period so that any Docker stop event that was already buffered before the
// hold was cleared does not slip through and trigger a spurious reconciliation
// restart.
type schedulerHoldEntry struct {
	count     int
	expiresAt time.Time // non-zero only when count == 0; suppression stays active until then
}

// schedulerStopHoldGracePeriod is how long the hold remains active after the
// last job releases it. The Docker event stream is asynchronous: the stop event
// can be buffered in the reconciliation channel for up to several hundred
// milliseconds after the scheduler has already restarted the service, so this
// grace period ensures those stale events are still suppressed.
const schedulerStopHoldGracePeriod = 10 * time.Second

type reconciliation struct {
	m sync.Mutex

	repoJobs              map[string]*job
	deployingStacks       map[string]int
	schedulerHeldServices map[string]schedulerHoldEntry // key = "context/project/service"
}

func newReconciliation() *reconciliation {
	return &reconciliation{
		repoJobs:              make(map[string]*job),
		deployingStacks:       make(map[string]int),
		schedulerHeldServices: make(map[string]schedulerHoldEntry),
		m:                     sync.Mutex{},
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
	r.schedulerHeldServices = make(map[string]schedulerHoldEntry)
}

// MarkSchedulerStopHeld records that the scheduler has intentionally stopped the
// given compose service (identified by its Docker context, compose project, and
// service name) so that the reconciliation event listener does not try to
// restart it while the scheduled job is running. The hold is refcounted to
// handle concurrent jobs that stop the same service. contextName is the
// normalized Docker context name (empty string = default context) the
// service lives on; the same project/service name on two different contexts
// are tracked independently.
func MarkSchedulerStopHeld(contextName, project, service string) {
	reconciliationHandler.markSchedulerStopHeld(contextName, project, service)
}

// UnmarkSchedulerStopHeld releases a hold previously registered via
// MarkSchedulerStopHeld. When the refcount reaches zero the hold enters a
// grace period (see schedulerStopHoldGracePeriod) so that any Docker stop
// event still buffered in the reconciliation channel does not trigger a
// spurious restart.
func UnmarkSchedulerStopHeld(contextName, project, service string) {
	reconciliationHandler.unmarkSchedulerStopHeld(contextName, project, service)
}

func schedulerHeldServiceKey(contextName, project, service string) string {
	contextName = docker.NormalizeContextName(contextName)
	return contextName + "/" + project + "/" + service
}

func (r *reconciliation) markSchedulerStopHeld(contextName, project, service string) {
	if project == "" || service == "" {
		return
	}

	key := schedulerHeldServiceKey(contextName, project, service)

	r.m.Lock()
	entry := r.schedulerHeldServices[key]
	entry.count++
	entry.expiresAt = time.Time{} // clear any lingering grace period
	r.schedulerHeldServices[key] = entry
	r.m.Unlock()
}

func (r *reconciliation) unmarkSchedulerStopHeld(contextName, project, service string) {
	if project == "" || service == "" {
		return
	}

	key := schedulerHeldServiceKey(contextName, project, service)

	r.m.Lock()
	defer r.m.Unlock()

	entry := r.schedulerHeldServices[key]

	if entry.count <= 1 {
		// Last holder released. Keep the entry alive for the grace period so
		// that stop events already buffered in the reconciliation channel are
		// still suppressed after the service has been restarted.
		r.schedulerHeldServices[key] = schedulerHoldEntry{
			count:     0,
			expiresAt: time.Now().Add(schedulerStopHoldGracePeriod),
		}
	} else {
		entry.count--
		r.schedulerHeldServices[key] = entry
	}
}

// isServiceSchedulerStopHeld reports whether the container described by attrs is
// currently held stopped by the job scheduler (or within the post-release grace
// period) on the given Docker context. It uses the standard Docker Compose
// labels to identify the service.
func (r *reconciliation) isServiceSchedulerStopHeld(contextName string, attrs map[string]string) bool {
	if attrs == nil {
		return false
	}

	project := strings.TrimSpace(attrs[api.ProjectLabel])
	service := strings.TrimSpace(attrs[api.ServiceLabel])

	if project == "" || service == "" {
		return false
	}

	key := schedulerHeldServiceKey(contextName, project, service)

	r.m.Lock()
	defer r.m.Unlock()

	entry, ok := r.schedulerHeldServices[key]
	if !ok {
		return false
	}

	if entry.count > 0 {
		return true
	}

	// Grace period: still suppress events shortly after the service was restarted.
	if !entry.expiresAt.IsZero() && time.Now().Before(entry.expiresAt) {
		return true
	}

	// Grace period expired — clean up lazily.
	delete(r.schedulerHeldServices, key)

	return false
}

// stackDeploymentKey builds the tracking key for a deployment. Stack names are
// only guaranteed unique within a Docker context, so context is included to
// avoid conflating same-named stacks deployed to different contexts.
func stackDeploymentKey(repository, context, stack string) string {
	context = docker.NormalizeContextName(context)
	return repository + "/" + context + "/" + stack
}

func (r *reconciliation) startStackDeployment(repository, context, stack string) {
	if repository == "" || stack == "" {
		return
	}

	key := stackDeploymentKey(repository, context, stack)

	r.m.Lock()
	r.deployingStacks[key]++
	r.m.Unlock()
}

func (r *reconciliation) finishStackDeployment(repository, context, stack string) {
	if repository == "" || stack == "" {
		return
	}

	key := stackDeploymentKey(repository, context, stack)

	r.m.Lock()
	defer r.m.Unlock()

	count := r.deployingStacks[key]
	if count <= 1 {
		delete(r.deployingStacks, key)
		return
	}

	r.deployingStacks[key] = count - 1
}

func (r *reconciliation) isStackDeploymentInProgress(repository, context, stack string) bool {
	if repository == "" || stack == "" {
		return false
	}

	key := stackDeploymentKey(repository, context, stack)

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

	jobLog := info.jobLog

	go func() {
		defer func() {
			if r := recover(); r != nil {
				jobLog.Error("reconciliation job panicked", slog.Any("recover", r))
			}
		}()

		newJob.run(context.WithoutCancel(ctx))
	}()
}

func getDeployConfigGroupByEvent(dcs []*deployConfig.Config) map[string][]*deployConfig.Config {
	m := make(map[string][]*deployConfig.Config)

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
