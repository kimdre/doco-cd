package reconciliation

import (
	"fmt"
	"log/slog"
	"slices"
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
		old := m.pauses.repos[req.Repository.Name]
		m.pauses.mu.Unlock()

		if old != nil && predatesJob(req, old.request) {
			return 0
		}

		snapshot := req

		snapshot.DeployConfigs = make([]*deployConfig.Config, len(req.DeployConfigs))
		for i, dc := range req.DeployConfigs {
			snapshot.DeployConfigs[i] = clone.New(dc)
		}

		snapshot.AutoDiscoveryOrigins = *clone.New(&req.AutoDiscoveryOrigins)

		m.pauses.mu.Lock()
		if m.pauses.repos[req.Repository.Name] != old {
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

				if pause, owned := deployConfig.PauseForAutoDiscoveryOrigin(dc, snapshot.AutoDiscoveryOrigins); owned {
					observed.scopes[key] = pause
					observed.targets[key] = dc
				}
			}
		}

		m.pauses.repos[req.Repository.Name] = observed
		m.pauses.mu.Unlock()

		return observed.generation
	}
}

func (m *Manager) pauseForLocked(req DeployRequest, dc *deployConfig.Config) bool {
	if current := m.pauses.repos[req.Repository.Name]; current != nil {
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

	if current := m.pauses.repos[req.Repository.Name]; current != nil {
		return current.request
	}

	return req
}

func (m *Manager) removalPredicate(req DeployRequest, contextName string, gate *syncWindowGate) func(*slog.Logger, string) bool {
	return func(log *slog.Logger, name string) bool {
		dc := &deployConfig.Config{Name: name, Context: contextName}
		if m.pauseFor(req, dc) {
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
	if current := m.pauses.repos[req.Repository.Name]; current != nil {
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

	current := m.pauses.repos[req.Repository.Name]

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

		if observed.scopes[stackDeploymentKey(req.Repository.Name, contextName, name)] {
			return false
		}
	}

	return true
}
