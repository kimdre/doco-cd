package reconciliation

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/common/types/clone"
	"github.com/kimdre/doco-cd/internal/config"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

type pauseObservation struct {
	request    DeployRequest
	generation uint64
	scopes     map[string]bool
	targets    map[string]*deployConfig.Config
}

type pauseRegistry struct {
	mu         sync.Mutex
	generation uint64
	repos      map[string]*pauseObservation
}

func (m *Manager) observePause(req DeployRequest) uint64 {
	for {
		m.pauses.mu.Lock()
		old := m.pauses.repos[req.key()]
		m.pauses.mu.Unlock()

		if old != nil && predatesJob(req, old.request) {
			return 0
		}

		matches := func(filter string) bool {
			return stages.WebhookEventFilterMatches(req.JobTrigger, filter, req.Payload)
		}

		// A webhook that a filter excludes keeps the previous pause and reconciliation intent.
		// The configs stay in the snapshot, so cleanup still treats their stacks as present.
		snapshot := req

		snapshot.DeployConfigs = make([]*deployConfig.Config, len(req.DeployConfigs))
		for i, dc := range req.DeployConfigs {
			snapshot.DeployConfigs[i] = clone.New(dc)

			if old != nil && !matches(dc.WebhookEventFilter) {
				if previous := old.targets[stackDeploymentKey(req.Repository.Name, dc.Context, dc.Name)]; previous != nil {
					snapshot.DeployConfigs[i].Pause = previous.Pause
					snapshot.DeployConfigs[i].Reconciliation.Enabled = previous.Reconciliation.Enabled
				}
			}
		}

		snapshot.AutoDiscoveryOrigins = *clone.New(&req.AutoDiscoveryOrigins)
		for i, origin := range snapshot.AutoDiscoveryOrigins {
			if old == nil || matches(origin.WebhookEventFilter) {
				continue
			}

			for _, previous := range old.request.AutoDiscoveryOrigins {
				if previous.SameOwner(origin) {
					snapshot.AutoDiscoveryOrigins[i].Pause = previous.Pause
					snapshot.AutoDiscoveryOrigins[i].ReconciliationEnabled = previous.ReconciliationEnabled

					break
				}
			}
		}

		m.pauses.mu.Lock()
		if m.pauses.repos[req.key()] != old {
			m.pauses.mu.Unlock()
			continue
		}

		if m.pauses.repos == nil {
			m.pauses.repos = make(map[string]*pauseObservation)
		}

		m.pauses.generation++

		observed := &pauseObservation{
			request: snapshot, generation: m.pauses.generation,
			scopes:  make(map[string]bool, len(req.DeployConfigs)),
			targets: make(map[string]*deployConfig.Config, len(req.DeployConfigs)),
		}
		for _, dc := range snapshot.DeployConfigs {
			key := stackDeploymentKey(req.Repository.Name, dc.Context, dc.Name)
			observed.scopes[key] = dc.Pause
			observed.targets[key] = dc

			var before bool
			if old != nil {
				before = old.scopes[key]
			}

			if before != dc.Pause && req.Logger != nil {
				req.Logger.Info("deployment pause changed", slog.String("stack", dc.Name),
					slog.String("context", dc.Context),
					slog.Bool("pause", dc.Pause))
			}
		}

		if old != nil {
			for key, dc := range old.targets {
				if _, present := observed.targets[key]; present {
					continue
				}

				// A stack missing from a filtered webhook's configs keeps its previous intent.
				if !matches(dc.WebhookEventFilter) {
					observed.scopes[key] = dc.Pause
					observed.targets[key] = dc

					continue
				}

				if pause, reconciliation, owned := deployConfig.ControlsForAutoDiscoveryOrigin(dc, snapshot.AutoDiscoveryOrigins); owned {
					dc = clone.New(dc)
					dc.Pause = pause
					dc.Reconciliation.Enabled = reconciliation
					observed.scopes[key] = pause
					observed.targets[key] = dc
				}
			}
		}

		m.pauses.repos[req.key()] = observed
		m.pauses.mu.Unlock()

		return observed.generation
	}
}

func (m *Manager) pauseForLocked(req DeployRequest, dc *deployConfig.Config) bool {
	if current := m.pauses.repos[req.key()]; current != nil {
		if pause, present := current.scopes[stackDeploymentKey(req.Repository.Name, dc.Context, dc.Name)]; present {
			return pause
		}

		pause, _ := deployConfig.PauseForAutoDiscoveryOrigin(dc, current.request.AutoDiscoveryOrigins)

		return pause
	}

	return dc.Pause
}

func (m *Manager) withPausedRootConfigs(req DeployRequest, previous *job) DeployRequest {
	if previous == nil {
		return req
	}

	configs := slices.Clone(req.DeployConfigs)
	for _, dc := range append(slices.Clone(previous.info.DeployConfigs), previous.pinned...) {
		if !m.pauseFor(req, dc) || slices.ContainsFunc(configs, func(current *deployConfig.Config) bool {
			return sameStack(current, dc)
		}) {
			continue
		}

		configs = append(configs, dc)
	}

	req.DeployConfigs = configs

	return req
}

func (m *Manager) pauseFor(req DeployRequest, dc *deployConfig.Config) bool {
	m.pauses.mu.Lock()
	defer m.pauses.mu.Unlock()

	return m.pauseForLocked(req, dc)
}

// The locked check is the admission point. A later pause does not cancel work
// that has already passed it.
func (m *Manager) admitPause(req DeployRequest, dc *deployConfig.Config, rev *stackRevision) error {
	m.pauses.mu.Lock()
	defer m.pauses.mu.Unlock()

	pause := m.pauseForLocked(req, dc)
	if req.Origin == syncwindow.OriginReconciliation {
		if !m.reconciliationEnabledLocked(req, dc) {
			return fmt.Errorf("reconciliation disabled: %w", stages.ErrSkipDeployment)
		}

		if pause && rev != nil && !rev.restoresDeployed() {
			return stages.DeploymentPaused()
		}
	} else if pause {
		return stages.DeploymentPaused()
	}

	return nil
}

func (m *Manager) currentCleanupRequest(req DeployRequest) DeployRequest {
	m.pauses.mu.Lock()
	defer m.pauses.mu.Unlock()

	if current := m.pauses.repos[req.key()]; current != nil &&
		cleanupRetryKey(current.request) == cleanupRetryKey(req) {
		request := current.request
		request.pauseGeneration = current.generation

		return request
	}

	return req
}

func (m *Manager) removalPredicate(req DeployRequest, contextName string, gate *syncWindowGate) func(*slog.Logger, string) bool {
	return func(log *slog.Logger, name string) bool {
		if m.cleanupRetrySuperseded(req) {
			log.Debug("removal skipped because a newer source request superseded cleanup")
			return false
		}

		dc := &deployConfig.Config{Name: name, Context: contextName}

		m.pauses.mu.Lock()

		paused := slices.ContainsFunc(req.DeployConfigs, func(cfg *deployConfig.Config) bool {
			return sameStack(cfg, dc) && cfg.Pause
		})
		if current := m.pauses.repos[req.key()]; current != nil &&
			cleanupRetryKey(current.request) == cleanupRetryKey(req) {
			paused = m.pauseForLocked(req, dc)
		}
		m.pauses.mu.Unlock()

		if paused {
			log.Debug("removal skipped because deployment is paused")
			return false
		}

		return gate.allowRemoval(log, contextName, name)
	}
}

func (m *Manager) recoveryConfigs(req DeployRequest, configs []*deployConfig.Config) []*deployConfig.Config {
	m.pauses.mu.Lock()
	defer m.pauses.mu.Unlock()

	result := make([]*deployConfig.Config, 0, len(configs))
	for _, dc := range configs {
		if !dc.Reconciliation.Enabled || !m.reconciliationEnabledLocked(req, dc) {
			continue
		}

		result = append(result, dc)
	}

	return result
}

func (m *Manager) reconciliationEnabledLocked(req DeployRequest, dc *deployConfig.Config) bool {
	if current := m.pauses.repos[req.key()]; current != nil {
		if target := current.targets[stackDeploymentKey(req.Repository.Name, dc.Context, dc.Name)]; target != nil {
			return target.Reconciliation.Enabled
		}
	}

	return dc.Reconciliation.Enabled
}

func (m *Manager) activeDeployConfigs(req DeployRequest) []*deployConfig.Config {
	var configs []*deployConfig.Config

	for _, dc := range req.DeployConfigs {
		if m.admitPause(req, dc, nil) == nil {
			configs = append(configs, dc)
		}
	}

	return configs
}

func (m *Manager) recoveryGroupByEvent(req DeployRequest, configs []*deployConfig.Config) map[string][]*deployConfig.Config {
	if m == nil {
		return getDeployConfigGroupByEvent(configs)
	}

	return getDeployConfigGroupByEvent(m.recoveryConfigs(req, configs))
}

func (m *Manager) newerPauseObservation(req DeployRequest) bool {
	m.pauses.mu.Lock()
	defer m.pauses.mu.Unlock()

	current := m.pauses.repos[req.key()]

	return current != nil && current.generation > req.pauseGeneration
}

func (m *Manager) hasPause(req DeployRequest) bool {
	current := m.currentCleanupRequest(req)
	for _, dc := range current.DeployConfigs {
		if dc.Pause {
			return true
		}
	}

	for _, origin := range current.AutoDiscoveryOrigins {
		if origin.Pause {
			return true
		}
	}

	return false
}

// AdmitCertificateRotation checks current deployment intent before certificates are reissued.
func (m *Manager) AdmitCertificateRotation(contextName string, labels map[string]string) bool {
	name := labels[docker.DocoCDLabels.Deployment.Name]
	if name == "" {
		name = labels[api.ProjectLabel]
	}

	m.pauses.mu.Lock()
	defer m.pauses.mu.Unlock()

	sourceName := labels[docker.DocoCDLabels.Source.Name]
	sourceURL := labels[docker.DocoCDLabels.Source.URL]

	for _, observed := range m.pauses.repos {
		req := observed.request
		matchesName := sourceName != "" && sourceName == req.Repository.Name

		matchesURL := sourceURL != "" && (sourceURL == req.Repository.ConfigSourceUrl || sourceURL == req.Repository.SourceUrl)
		if sourceURL != "" && req.Repository.Source != config.SourceTypeOCI {
			sourceIdentity := git.GetRepoName(sourceURL)
			matchesURL = matchesURL || sourceIdentity != "" && (sourceIdentity == git.GetRepoName(req.Repository.ConfigSourceUrl) ||
				sourceIdentity == git.GetRepoName(req.Repository.SourceUrl))
		}

		// Older labels may lack the source URL. The payload name then identifies the source.
		matchesPayloadName := sourceURL == "" && sourceName != "" && req.Payload != nil && sourceName == req.Payload.FullName
		if !matchesName && !matchesURL && !matchesPayloadName {
			continue
		}

		if pause, present := observed.scopes[stackDeploymentKey(req.Repository.Name, contextName, name)]; present {
			if pause {
				return false
			}

			continue
		}

		// Stacks no longer found in Git stay protected by a paused discovery root, as in cleanup.
		cleanup := newObsoleteStackCleanup(slog.Default(), nil, false, contextName, req, nil)
		for _, origin := range cleanup.owners(labels[docker.DocoCDLabels.Deployment.WorkingDir],
			strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.ConfigTarget])) {
			if origin.Pause {
				return false
			}
		}
	}

	return true
}
