package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/graceful"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

// contextRefreshInterval controls how often Manager re-lists the Docker
// context registry to pick up newly available contexts (e.g. one that was
// previously unreachable) and start a worker for them. Contexts that are
// already running a worker are left untouched.
const contextRefreshInterval = time.Minute

// Manager runs one Compose worker for every Docker context and, when a context
// is a Swarm manager, an additional Swarm worker. It exposes context-aware
// ListJobs/TriggerNow for REST wiring and can be constructed even when
// automatic scheduling is disabled.
type Manager struct {
	registry        *docker.ContextRegistry
	log             *slog.Logger
	wg              *sync.WaitGroup
	secretProvider  secretprovider.SecretProvider
	stopHoldTracker ServiceStopHoldTracker
	runtime         *runtimeStore

	mu      sync.Mutex
	workers map[string]managedWorker // key = normalized context name + runtime mode
	nextID  uint64
}

type managedWorker struct {
	cancel context.CancelFunc
	mode   scheduledJobMode
	// id prevents a stopped worker from deleting a replacement using the same key.
	id uint64
	// stopping keeps the worker registered until its active runs have drained.
	stopping bool
}

// NewManager creates a scheduler Manager bound to registry. log and wg are
// required for Start (running background workers) but may be omitted if the
// Manager is only used for on-demand ListJobs/TriggerNow calls.
func NewManager(registry *docker.ContextRegistry, log *slog.Logger, wg *sync.WaitGroup, secretProvider secretprovider.SecretProvider, stopHoldTracker ServiceStopHoldTracker) *Manager {
	if log == nil {
		log = slog.Default()
	}

	return &Manager{
		registry:        registry,
		log:             log.With(slog.String("component", "scheduler_manager")),
		wg:              wg,
		secretProvider:  secretProvider,
		stopHoldTracker: stopHoldTracker,
		runtime:         newRuntimeStore(),
		workers:         map[string]managedWorker{},
	}
}

// Start launches the required worker(s) for every Docker context currently
// known to the registry, and periodically re-lists the registry to discover
// new contexts.
func (m *Manager) Start(ctx context.Context) {
	if m == nil || m.registry == nil || m.wg == nil {
		return
	}

	m.refreshWorkers(ctx)

	graceful.SafeGo(m.wg, m.log, func() {
		defer m.stopWorkers()

		ticker := time.NewTicker(contextRefreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.refreshWorkers(ctx)
			}
		}
	})
}

// refreshWorkers reconciles scheduler workers with the current context list.
// Unavailable contexts do not disturb existing workers; a changed capability
// adds/removes only the Swarm worker while keeping the Compose worker alive.
func (m *Manager) refreshWorkers(ctx context.Context) {
	results, err := m.registry.List(ctx)
	if err != nil {
		m.log.Error("failed to list docker contexts for scheduler", logger.ErrAttr(err))
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	available := make(map[string]struct{}, len(results)*2)
	for _, result := range results {
		name := docker.NormalizeContextName(result.Name)
		if result.Err != nil {
			// Keep already-running workers alive while a transient context probe
			// fails; only an actually removed context should stop them.
			for key := range m.workers {
				if workerContextFromKey(key) == name {
					available[key] = struct{}{}
				}
			}

			m.log.Warn("skipping unavailable docker context for scheduler",
				slog.String("context", docker.DisplayContextName(name)),
				logger.ErrAttr(result.Err),
			)

			continue
		}

		for _, mode := range schedulerModes(result.SwarmMode) {
			key := schedulerWorkerKey(name, mode)

			available[key] = struct{}{}
			if _, hasWorker := m.workers[key]; hasWorker {
				continue
			}

			worker := newSchedulerForMode(result.ContextClient, mode, m.log, m.wg, m.secretProvider, m.stopHoldTracker, m.runtime)
			workerCtx, cancel := context.WithCancel(ctx)
			m.nextID++
			workerID := m.nextID
			m.workers[key] = managedWorker{cancel: cancel, mode: mode, id: workerID}

			graceful.SafeGo(m.wg, m.log, func() {
				defer m.workerStopped(key, workerID)

				worker.run(workerCtx)
			})
		}
	}

	for key, worker := range m.workers {
		if _, ok := available[key]; ok {
			continue
		}

		if worker.stopping {
			continue
		}

		worker.cancel()
		worker.stopping = true
		m.workers[key] = worker
		m.log.Info("stopped scheduler worker",
			slog.String("context", docker.DisplayContextName(workerContextFromKey(key))),
			slog.String("mode", string(worker.mode)),
		)
	}
}

// ListJobs returns all discovered scheduler jobs for the given Docker context
// (normalized/display form accepted, empty means the default context),
// optionally filtered by stack name.
func (m *Manager) ListJobs(ctx context.Context, contextName, stackName string) ([]JobInfo, error) {
	if m == nil || m.registry == nil {
		return nil, errors.New("scheduler manager is not configured with a docker context registry")
	}

	cc, err := m.registry.Get(ctx, contextName)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve docker context %q: %w", docker.DisplayContextName(contextName), err)
	}

	return listJobsForModes(ctx, schedulerModes(cc.SwarmMode), cc, m.log, m.secretProvider, m.runtime, stackName)
}

// TriggerNow executes one configured scheduled job immediately on the given
// Docker context. Job selection matches by container/service name and
// optional stack name.
func (m *Manager) TriggerNow(ctx context.Context, contextName, jobName, stackName string, secretProvider secretprovider.SecretProvider) (string, error) {
	if m == nil || m.registry == nil {
		return "", errors.New("scheduler manager is not configured with a docker context registry")
	}

	cc, err := m.registry.Get(ctx, contextName)
	if err != nil {
		return "", fmt.Errorf("failed to resolve docker context %q: %w", docker.DisplayContextName(contextName), err)
	}

	return triggerNowForModes(ctx, schedulerModes(cc.SwarmMode), cc, m.log, jobName, stackName, secretProvider, m.stopHoldTracker, m.runtime)
}

// stopWorkers requests cancellation for every managed worker. Each worker
// removes its own runtime partition after its active runs have drained.
func (m *Manager) stopWorkers() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, worker := range m.workers {
		if worker.stopping {
			continue
		}

		worker.cancel()
		worker.stopping = true
		m.workers[key] = worker
	}
}

func (m *Manager) workerStopped(key string, id uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	worker, ok := m.workers[key]
	if !ok || worker.id != id {
		return
	}

	m.runtime.clearContextMode(workerContextFromKey(key), worker.mode)
	delete(m.workers, key)
}

func schedulerModes(swarmAvailable bool) []scheduledJobMode {
	modes := []scheduledJobMode{scheduledJobModeContainer}
	if swarmAvailable {
		modes = append(modes, scheduledJobModeSwarm)
	}

	return modes
}

func schedulerWorkerKey(contextName string, mode scheduledJobMode) string {
	return docker.NormalizeContextName(contextName) + "\x00" + string(mode)
}

func workerContextFromKey(key string) string {
	contextName, _, _ := strings.Cut(key, "\x00")
	return contextName
}
