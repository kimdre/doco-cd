package reconciliation

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

// syncWindowGate applies the sync window policy to one deployment request.
// Every stack of the request is evaluated once, at the same point in time, so
// a run admitted inside a window finishes even if the window closes while it
// waits for deployment admission.
//
// A nil gate allows everything.
type syncWindowGate struct {
	manager    *Manager
	policy     *syncwindow.Policy
	now        time.Time
	origin     syncwindow.Origin
	repository stages.RepositoryData
	// restoreOnly marks the gate of a reconciliation deployment. It only
	// applies to stacks that would not be restored to a revision known to be
	// deployed, see stackRevision.restoresDeployed.
	restoreOnly bool
	// decisions holds the decisions of stacks that are blocked or only
	// allowed because of manual_sync. Stacks missing here are allowed.
	decisions map[*deployConfig.Config]syncwindow.Decision

	mu       sync.Mutex
	outcomes map[*deployConfig.Config]error
}

// newSyncWindowGate evaluates the sync window policy for every deploy config
// of req. It returns nil if no sync windows are configured.
func (m *Manager) newSyncWindowGate(req DeployRequest, now time.Time) *syncWindowGate {
	if m == nil || m.appConfig == nil || m.appConfig.SyncWindows.Empty() {
		return nil
	}

	origin := req.Origin
	restoreOnly := origin == syncwindow.OriginReconciliation

	// A reconciliation deployment that would change the revision of a stack
	// is not a recovery, so it is gated like an automatic deployment.
	if origin == "" || restoreOnly {
		origin = syncwindow.OriginAutomatic
	}

	gate := &syncWindowGate{
		manager:     m,
		policy:      m.appConfig.SyncWindows,
		now:         now,
		origin:      origin,
		repository:  req.Repository,
		restoreOnly: restoreOnly,
		decisions:   make(map[*deployConfig.Config]syncwindow.Decision),
		outcomes:    make(map[*deployConfig.Config]error),
	}

	for _, dc := range req.DeployConfigs {
		if dc == nil {
			continue
		}

		decision := gate.policy.Evaluate(now, syncWindowTarget(req.Repository, dc), origin)
		if !decision.Allowed || decision.ManualOverride {
			gate.decisions[dc] = decision
		}
	}

	return gate
}

// syncWindowTarget returns the sync window target of a deploy config.
func syncWindowTarget(repository stages.RepositoryData, dc *deployConfig.Config) syncwindow.Target {
	target := syncwindow.Target{
		Repository: repository.Name,
		Deployment: dc.Name,
		Context:    docker.DisplayContextName(dc.Context),
	}

	// Deploy configs with their own repository_url deploy from that repository.
	if repositoryURL := strings.TrimSpace(string(dc.RepositoryUrl)); repositoryURL != "" {
		target.Repository = git.GetRepoName(repositoryURL)
		target.ConfigRepository = repository.Name
	}

	return target
}

// blocks reports whether a sync window blocks the deployment of dc at the
// revision of the request.
func (g *syncWindowGate) blocks(dc *deployConfig.Config) bool {
	if g == nil || g.restoreOnly {
		return false
	}

	decision, ok := g.decisions[dc]

	return ok && !decision.Allowed
}

// admit returns a *stages.SyncWindowBlockedError if a sync window blocks the
// deployment of dc at the revision of the request. See admitStack.
func (g *syncWindowGate) admit(log *slog.Logger, dc *deployConfig.Config, postStatus func(description string)) error {
	if g == nil {
		return nil
	}

	return g.admitStack(log, dc, stackRevision{revision: g.repository.Revision}, postStatus)
}

// admitStack returns a *stages.SyncWindowBlockedError if a sync window blocks
// deploying rev of dc. postStatus, if not nil, posts the pending commit status
// of a deferred deployment. It is only called the first time a revision of the
// stack is deferred, to avoid repeating it on every poll.
func (g *syncWindowGate) admitStack(log *slog.Logger, dc *deployConfig.Config, rev stackRevision,
	postStatus func(description string),
) error {
	if g == nil {
		return nil
	}

	if g.restoreOnly && rev.restoresDeployed() {
		return nil
	}

	decision, ok := g.decisions[dc]
	if !ok {
		return nil
	}

	if decision.Allowed {
		log.Info("sync window bypassed by manual deployment", slog.Any("sync_windows", decision.Windows))

		return nil
	}

	g.countBlocked(dc.Name, dc.Context, decision.Windows)

	attrs := syncWindowLogAttrs(decision)
	if g.manager.syncWindowNotices.first(stackDeploymentKey(g.repository.Name, dc.Context, dc.Name), rev.revision) {
		log.Info("deployment deferred by sync window", attrs...)

		if postStatus != nil {
			postStatus(stages.SyncWindowCommitStatusDescription(decision.NextOpen))
		}
	} else {
		log.Debug("deployment deferred by sync window", attrs...)
	}

	return &stages.SyncWindowBlockedError{
		Stacks:   []string{dc.Name},
		Windows:  decision.Windows,
		NextOpen: decision.NextOpen,
	}
}

// stackRevision is the revision a deployment of a stack deploys.
type stackRevision struct {
	// revision is the revision the stack deploys.
	revision string
	// deployed is the revision the stack's containers are labeled with, if known.
	deployed string
	// own reports whether revision was resolved from the stack's own
	// reference, repository_url or git_depth instead of being the revision of
	// the request.
	own bool
}

// restoresDeployed reports whether deploying r restores a revision known to be
// deployed. A reconciliation request always carries the revision its stacks
// were last deployed or found unchanged with. A stack that resolves its own
// reference instead may get a newer revision, so it only restores the deployed
// revision if its containers are labeled with it.
func (r stackRevision) restoresDeployed() bool {
	if !r.own {
		return true
	}

	revision := strings.TrimSpace(r.revision)

	return revision != "" && revision == strings.TrimSpace(r.deployed)
}

// allowRemoval reports whether an obsolete auto-discovered stack may be
// removed. Removing a stack changes it just like a deployment does.
func (g *syncWindowGate) allowRemoval(log *slog.Logger, contextName, stackName string) bool {
	if g == nil {
		return true
	}

	target := syncwindow.Target{
		Repository: g.repository.Name,
		Deployment: stackName,
		Context:    docker.DisplayContextName(contextName),
	}

	decision := g.policy.Evaluate(g.now, target, g.origin)
	if decision.Allowed {
		if decision.ManualOverride {
			log.Info("sync window bypassed by manual deployment", slog.Any("sync_windows", decision.Windows))
		}

		return true
	}

	g.countBlocked(stackName, contextName, decision.Windows)

	attrs := syncWindowLogAttrs(decision)
	if g.manager.syncWindowNotices.first("removal:"+stackDeploymentKey(g.repository.Name, contextName, stackName), g.repository.Revision) {
		log.Info("removal of obsolete auto-discovered stack deferred by sync window", attrs...)
	} else {
		log.Debug("removal of obsolete auto-discovered stack deferred by sync window", attrs...)
	}

	return false
}

// removalPredicate returns the predicate passed to
// cleanupObsoleteAutoDiscoveredContainers, or nil if everything is allowed.
func (g *syncWindowGate) removalPredicate(contextName string) func(log *slog.Logger, stackName string) bool {
	if g == nil {
		return nil
	}

	return func(log *slog.Logger, stackName string) bool {
		return g.allowRemoval(log, contextName, stackName)
	}
}

func (g *syncWindowGate) countBlocked(stackName, contextName string, windows []string) {
	for _, window := range windows {
		prometheus.SyncWindowBlockedTotal.WithLabelValues(
			resolveDeploymentQueueRepository(g.repository.Name),
			stackName,
			docker.DisplayContextName(contextName),
			window,
		).Inc()
	}
}

// record stores the outcome of the deployment of dc.
func (g *syncWindowGate) record(dc *deployConfig.Config, err error) {
	if g == nil {
		return
	}

	g.mu.Lock()
	g.outcomes[dc] = err
	g.mu.Unlock()

	// The stack now matches its revision, so a later deferral is new again.
	// A reconciliation only restores the deployed revision, the deferred
	// revision is still pending.
	if !g.restoreOnly && (err == nil || isUnchangedOutcome(err)) {
		g.manager.syncWindowNotices.clear(stackDeploymentKey(g.repository.Name, dc.Context, dc.Name))
	}
}

// deferred returns the deploy configs whose revision in this request must not
// be used by reconciliation: all stacks a sync window blocked, except those
// that turned out to be unchanged. A deferred stack keeps the reconciliation
// state of the revision that is actually deployed (see Manager.addJob).
func (g *syncWindowGate) deferred() map[*deployConfig.Config]struct{} {
	// Reconciliation requests do not replace reconciliation jobs.
	if g == nil || g.restoreOnly {
		return nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	deferred := make(map[*deployConfig.Config]struct{})

	for dc, decision := range g.decisions {
		if decision.Allowed {
			continue
		}

		outcome, recorded := g.outcomes[dc]
		if recorded && isUnchangedOutcome(outcome) {
			continue
		}

		deferred[dc] = struct{}{}
	}

	return deferred
}

// isUnchangedOutcome reports whether err means the stack was already up to date.
func isUnchangedOutcome(err error) bool {
	return errors.Is(err, stages.ErrSkipDeployment) &&
		!errors.Is(err, stages.ErrSyncWindowBlocked) &&
		!errors.Is(err, stages.ErrWebhookFilterMismatch)
}

func syncWindowLogAttrs(decision syncwindow.Decision) []any {
	attrs := []any{slog.Any("sync_windows", decision.Windows)}
	if !decision.NextOpen.IsZero() {
		attrs = append(attrs, slog.Time("next_open", decision.NextOpen))
	}

	return attrs
}

// syncWindowNotices remembers which revision of a stack was last reported as
// deferred, so a poll does not repeat the same Info log and commit status
// every interval while a window stays closed.
type syncWindowNotices struct {
	mu   sync.Mutex
	seen map[string]string // key is the stack key, value is the revision last reported as deferred
}

// first reports whether revision of the stack identified by key is deferred
// for the first time, and remembers it.
func (n *syncWindowNotices) first(key, revision string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.seen == nil {
		n.seen = make(map[string]string)
	}

	if last, ok := n.seen[key]; ok && last == revision {
		return false
	}

	n.seen[key] = revision

	return true
}

// clear forgets the deferred revision of a stack after it was deployed.
func (n *syncWindowNotices) clear(key string) {
	n.mu.Lock()
	delete(n.seen, key)
	n.mu.Unlock()
}

// destroyHasNothingToRemove reports whether a destroy config targets a stack
// that no longer exists, in which case there is nothing a sync window could defer.
func destroyHasNothingToRemove(ctx context.Context, log *slog.Logger, apiClient client.APIClient, swarmMode bool, dc *deployConfig.Config) bool {
	labels, err := docker.GetServiceLabels(ctx, apiClient, swarmMode, dc.Name)
	if err != nil {
		log.Debug("failed to check whether the stack to destroy exists", slog.String("error", err.Error()))

		return false
	}

	return len(labels) == 0
}
