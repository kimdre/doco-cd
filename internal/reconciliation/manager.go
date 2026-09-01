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
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/app"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"

	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// ErrManagerClosed indicates that a deployment was submitted after shutdown began.
var ErrManagerClosed = errors.New("reconciliation manager is closed")

// Dependencies configures reconciliation lifecycle, deployment admission, and the stable
// application-level services shared by every deployment the Manager runs: application
// configuration, the data mount point, the base Docker CLI, the Docker context registry, and
// an optional secret provider. Per-run values (trigger, repository, deploy configs, payload,
// notification metadata) are supplied per call via DeployRequest instead.
type Dependencies struct {
	MaxConcurrentDeployments uint `validate:"min=1"`

	AppConfig      *app.Config          `validate:"required,nostructlevel"`
	DataMountPoint container.MountPoint `validate:"required"`
	DockerCLI      command.Cli          `validate:"required,nostructlevel"`
	// Contexts and SecretProvider are optional: resolveDeployContext falls back to the
	// default Docker context/CLI when Contexts is nil, and a nil SecretProvider means no
	// external secret provider is configured.
	Contexts       *docker.ContextRegistry
	SecretProvider secretprovider.SecretProvider
}

// Manager owns reconciliation jobs, active-deployment tracking, scheduler
// holds, and deployment admission state.
type Manager struct {
	jobs           jobRegistry
	deployments    deploymentTracker
	schedulerHolds schedulerHoldRegistry
	limiter        *DeployerLimiter
	closeOnce      sync.Once
	lifecycleMu    sync.Mutex
	closed         bool
	deployWG       sync.WaitGroup
	jobWG          sync.WaitGroup

	// Stable application dependencies shared by every deployment; see Dependencies.
	appConfig      *app.Config
	dataMountPoint container.MountPoint
	dockerCli      command.Cli
	contexts       *docker.ContextRegistry
	secretProvider secretprovider.SecretProvider
}

// NewManager validates dependencies and creates an isolated reconciliation manager.
func NewManager(dependencies Dependencies) (*Manager, error) {
	if dependencies.MaxConcurrentDeployments == 0 {
		dependencies.MaxConcurrentDeployments = 1
	}

	if err := validation.Validate(dependencies); err != nil {
		return nil, fmt.Errorf("validate reconciliation dependencies: %w", err)
	}

	return &Manager{
		jobs:           jobRegistry{jobs: make(map[string]*job)},
		deployments:    deploymentTracker{stacks: make(map[string]int)},
		schedulerHolds: schedulerHoldRegistry{services: make(map[string]schedulerHoldEntry)},
		limiter:        NewDeployerLimiter(dependencies.MaxConcurrentDeployments),
		appConfig:      dependencies.AppConfig,
		dataMountPoint: dependencies.DataMountPoint,
		dockerCli:      dependencies.DockerCLI,
		contexts:       dependencies.Contexts,
		secretProvider: dependencies.SecretProvider,
	}, nil
}

// contextCLIEntry holds a Docker CLI and its resolved metadata for one Docker context.
type contextCLIEntry struct {
	cli       command.Cli
	closeFn   func() // nil for the default context (which is always j.manager.dockerCli)
	swarmMode bool
}

// DeployRequest carries the per-run input for a single reconciliation deployment: the trigger
// metadata, source repository/payload data, resolved deploy configs, and (for test runs only) a
// unique test name. Stable application dependencies (app config, Docker CLI, context registry,
// secret provider) live on the Manager itself instead; see Dependencies.
type DeployRequest struct {
	Logger        *slog.Logger `validate:"required,nostructlevel"`
	Metadata      notification.Metadata
	JobTrigger    stages.JobTrigger `validate:"required,oneof=webhook poll"`
	Repository    stages.RepositoryData
	DeployConfigs []*deployConfig.Config `validate:"dive,required"`
	Payload       *webhook.ParsedPayload
	TestName      string
}

type job struct {
	manager                  *Manager
	info                     DeployRequest
	deployConfigGroupByEvent map[string][]*deployConfig.Config // key is the docker event action name (for example "die" or "unhealthy").
	restartStateMu           sync.Mutex                        // guards unhealthyRestartHistory and restartSuppressUntil against concurrent access from parallel per-context startup recovery goroutines.
	unhealthyRestartHistory  map[string][]time.Time            // key is the docker container ID, value is the list of timestamps of recent unhealthy restart events for that container.
	restartSuppressUntil     map[string]time.Time              // key is the docker container ID that was restarted, value is the timestamp until which follow-up events from that restart should be suppressed.
	closeChan                chan struct{}
	cancel                   context.CancelFunc
	readyChan                chan struct{}
	readyOnce                sync.Once
	closeOnce                sync.Once
	// contextCLIs maps context name (empty string = default) to its Docker CLI and metadata.
	// Populated at the start of run() and closed when the job exits.
	contextCLIs map[string]contextCLIEntry
}

func newJob(manager *Manager, info DeployRequest, deployConfigGroupByEvent map[string][]*deployConfig.Config) *job {
	return &job{
		manager:                  manager,
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

	j.closeOnce.Do(func() {
		if j.cancel != nil {
			j.cancel()
		}

		close(j.closeChan)
	})
}

func (j *job) signalReady() {
	if j == nil {
		return
	}

	j.readyOnce.Do(func() {
		j.info.Logger.Debug("reconciliation event listeners ready")
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

type jobRegistry struct {
	mu     sync.Mutex
	jobs   map[string]*job
	closed bool
}

// deploymentTracker records active stack deployments so matching Docker
// events do not start duplicate reconciliation work.
type deploymentTracker struct {
	mu     sync.Mutex
	stacks map[string]int
}

// schedulerHoldRegistry records Compose services intentionally stopped by
// scheduled jobs, including the post-release event grace period.
type schedulerHoldRegistry struct {
	mu       sync.Mutex
	services map[string]schedulerHoldEntry
}

// Close stops reconciliation jobs, waits for them to release Docker resources,
// and stops limiter cleanup. It is safe to call more than once.
func (m *Manager) Close() {
	if m == nil {
		return
	}

	m.closeOnce.Do(func() {
		m.lifecycleMu.Lock()
		m.closed = true
		m.lifecycleMu.Unlock()
		m.deployWG.Wait()

		jobs := m.jobs.removeAll()
		for _, job := range jobs {
			job.close()
		}

		m.jobWG.Wait()

		m.deployments.clear()
		m.schedulerHolds.clear()
		m.limiter.Close()
	})
}

func (r *jobRegistry) removeAll() []*job {
	r.mu.Lock()
	defer r.mu.Unlock()

	jobs := make([]*job, 0, len(r.jobs))
	for _, job := range r.jobs {
		jobs = append(jobs, job)
	}

	r.jobs = make(map[string]*job)
	r.closed = true

	return jobs
}

func (m *Manager) beginDeploy() error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	if m.closed {
		return ErrManagerClosed
	}

	m.deployWG.Add(1)

	return nil
}

// clear resets deployment state after all admitted work has drained.
func (r *deploymentTracker) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.stacks = make(map[string]int)
}

// clear releases scheduler hold state after scheduler workers have stopped.
func (r *schedulerHoldRegistry) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.services = make(map[string]schedulerHoldEntry)
}

// MarkSchedulerStopHeld records that the scheduler has intentionally stopped the
// given compose service (identified by its Docker context, compose project, and
// service name) so that the reconciliation event listener does not try to
// restart it while the scheduled job is running. The hold is refcounted to
// handle concurrent jobs that stop the same service. contextName is the
// normalized Docker context name (empty string = default context) the
// service lives on; the same project/service name on two different contexts
// are tracked independently.
func (m *Manager) MarkSchedulerStopHeld(contextName, project, service string) {
	m.schedulerHolds.mark(contextName, project, service)
}

// UnmarkSchedulerStopHeld releases a hold previously registered via
// MarkSchedulerStopHeld. When the refcount reaches zero the hold enters a
// grace period (see schedulerStopHoldGracePeriod) so that any Docker stop
// event still buffered in the reconciliation channel does not trigger a
// spurious restart.
func (m *Manager) UnmarkSchedulerStopHeld(contextName, project, service string) {
	m.schedulerHolds.unmark(contextName, project, service)
}

func schedulerHeldServiceKey(contextName, project, service string) string {
	contextName = docker.NormalizeContextName(contextName)
	return contextName + "/" + project + "/" + service
}

func (r *schedulerHoldRegistry) mark(contextName, project, service string) {
	if project == "" || service == "" {
		return
	}

	key := schedulerHeldServiceKey(contextName, project, service)

	r.mu.Lock()
	entry := r.services[key]
	entry.count++
	entry.expiresAt = time.Time{} // clear any lingering grace period
	r.services[key] = entry
	r.mu.Unlock()
}

func (r *schedulerHoldRegistry) unmark(contextName, project, service string) {
	if project == "" || service == "" {
		return
	}

	key := schedulerHeldServiceKey(contextName, project, service)

	r.mu.Lock()
	defer r.mu.Unlock()

	entry := r.services[key]

	if entry.count <= 1 {
		// Last holder released. Keep the entry alive for the grace period so
		// that stop events already buffered in the reconciliation channel are
		// still suppressed after the service has been restarted.
		r.services[key] = schedulerHoldEntry{
			count:     0,
			expiresAt: time.Now().Add(schedulerStopHoldGracePeriod),
		}
	} else {
		entry.count--
		r.services[key] = entry
	}
}

// isServiceSchedulerStopHeld reports whether the container described by attrs is
// currently held stopped by the job scheduler (or within the post-release grace
// period) on the given Docker context. It uses the standard Docker Compose
// labels to identify the service.
func (r *schedulerHoldRegistry) isHeld(contextName string, attrs map[string]string) bool {
	if attrs == nil {
		return false
	}

	project := strings.TrimSpace(attrs[api.ProjectLabel])
	service := strings.TrimSpace(attrs[api.ServiceLabel])

	if project == "" || service == "" {
		return false
	}

	key := schedulerHeldServiceKey(contextName, project, service)

	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.services[key]
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
	delete(r.services, key)

	return false
}

// stackDeploymentKey builds the tracking key for a deployment. Stack names are
// only guaranteed unique within a Docker context, so context is included to
// avoid conflating same-named stacks deployed to different contexts.
func stackDeploymentKey(repository, context, stack string) string {
	context = docker.NormalizeContextName(context)
	return repository + "/" + context + "/" + stack
}

func (r *deploymentTracker) start(repository, context, stack string) {
	if repository == "" || stack == "" {
		return
	}

	key := stackDeploymentKey(repository, context, stack)

	r.mu.Lock()
	r.stacks[key]++
	r.mu.Unlock()
}

func (r *deploymentTracker) finish(repository, context, stack string) {
	if repository == "" || stack == "" {
		return
	}

	key := stackDeploymentKey(repository, context, stack)

	r.mu.Lock()
	defer r.mu.Unlock()

	count := r.stacks[key]
	if count <= 1 {
		delete(r.stacks, key)
		return
	}

	r.stacks[key] = count - 1
}

func (r *deploymentTracker) isInProgress(repository, context, stack string) bool {
	if repository == "" || stack == "" {
		return false
	}

	key := stackDeploymentKey(repository, context, stack)

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.stacks[key] > 0
}

func (m *Manager) addJob(ctx context.Context, req DeployRequest) {
	cfg := getDeployConfigGroupByEvent(req.DeployConfigs)
	if len(cfg) == 0 {
		return
	}

	newJob := newJob(m, req, cfg)
	jobCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	newJob.cancel = cancel

	m.jobs.mu.Lock()
	if m.jobs.closed {
		m.jobs.mu.Unlock()
		newJob.close()

		return
	}

	m.jobWG.Add(1)
	old := m.jobs.jobs[req.Repository.Name]
	m.jobs.jobs[req.Repository.Name] = newJob
	m.jobs.mu.Unlock()

	old.close()

	jobLog := req.Logger

	go func() {
		defer func() {
			if r := recover(); r != nil {
				jobLog.Error("reconciliation job panicked", slog.Any("recover", r))
			}
		}()

		defer m.jobWG.Done()

		newJob.run(jobCtx)
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
