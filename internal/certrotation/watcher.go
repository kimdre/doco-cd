// Package certrotation implements a background watcher that automatically rotates certificates
// issued through rotation-capable external secret providers (currently only OpenBao PKI roles,
// see internal/secretprovider/openbao) before they expire.
//
// Deployments become eligible for automatic rotation when all of their certificate-bearing
// external secrets use a rotation-capable reference (e.g. OpenBao's "pki-role:" ref). This is
// recorded at deploy time via the cd.doco.deployment.cert.rotatable and
// cd.doco.deployment.cert.expiry Docker labels (see internal/docker/cert_rotation_labels.go).
package certrotation

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

// Watcher periodically checks all deployments carrying rotation-capable certificate labels and
// triggers reissuance + redeploy for any deployment whose certificate expiry falls within the
// configured rotation threshold.
//
// Rotation is performed independently for every locally configured Docker context (see
// docker.ContextRegistry), not just the default/local one: each context is discovered and
// processed on its own, using that context's own Docker client and Swarm-mode state, so a broken
// or unreachable context can never prevent rotation on the others. Contexts are processed
// sequentially rather than concurrently; this keeps rotation runs simple to reason about and
// avoids overlapping rotations without needing extra coordination, since each check interval is
// expected to comfortably fit a full sequential pass over all configured contexts.
type Watcher struct {
	contexts       *docker.ContextRegistry
	log            *slog.Logger
	secretProvider secretprovider.SecretProvider
	threshold      time.Duration
	checkInterval  time.Duration
	// certOptions bundles the Docker-owned settings needed to reload and redeploy the rotated
	// project (see docker.CertificateRotationOptions), resolved explicitly at composition time.
	certOptions docker.CertificateRotationOptions

	// now and listContexts are overridable in tests.
	now          func() time.Time
	listContexts func(ctx context.Context) ([]docker.ContextClientResult, error)
}

// New creates a Watcher. threshold is how far ahead of a certificate's expiry rotation is
// triggered; checkInterval is how often deployments are checked. contexts is used to discover and
// connect to every locally configured Docker context on each check.
func New(
	contexts *docker.ContextRegistry,
	log *slog.Logger,
	secretProvider secretprovider.SecretProvider,
	threshold, checkInterval time.Duration,
	certOptions docker.CertificateRotationOptions,
) *Watcher {
	w := &Watcher{
		contexts:       contexts,
		log:            log.With(slog.String("component", "certrotation")),
		secretProvider: secretProvider,
		threshold:      threshold,
		checkInterval:  checkInterval,
		certOptions:    certOptions,
		now:            time.Now,
	}

	if contexts != nil {
		w.listContexts = contexts.List
	}

	return w
}

// Start runs the watcher loop until ctx is cancelled. It performs an initial check immediately,
// then rechecks every checkInterval. Callers are expected to run Start in its own goroutine, e.g.
// via graceful.SafeGo, which handles WaitGroup bookkeeping.
func (w *Watcher) Start(ctx context.Context) {
	if w.contexts == nil || w.log == nil {
		return
	}

	w.log.Info("starting certificate rotation watcher",
		slog.String("threshold", formatWatcherDuration(w.threshold)),
		slog.String("check_interval", formatWatcherDuration(w.checkInterval)),
	)

	w.checkAndRotate(ctx)

	ticker := time.NewTicker(w.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("certificate rotation watcher stopped")
			return
		case <-ticker.C:
			w.checkAndRotate(ctx)
		}
	}
}

// checkAndRotate discovers every locally configured Docker context and processes each one
// independently: for each healthy context, it discovers all rotatable deployments in that
// context, and for each project whose certificate expiry is within the configured threshold (or
// whose certificate has been revoked), triggers a rotation redeploy against that context's own
// Docker client.
//
// Contexts are processed sequentially, and a failure discovering or checking one context (e.g. an
// unreachable remote Docker host) is logged and skipped without aborting the remaining contexts.
func (w *Watcher) checkAndRotate(ctx context.Context) {
	if w.listContexts == nil {
		return
	}

	results, err := w.listContexts(ctx)
	if err != nil {
		w.log.Error("failed to list docker contexts for certificate rotation", logger.ErrAttr(err))
		return
	}

	for _, result := range results {
		w.checkAndRotateContext(ctx, result)
	}
}

// checkAndRotateContext runs a single certificate-rotation check/redeploy pass against one Docker
// context. It never returns an error: failures are logged (tagged with the context's display
// name, see docker.DisplayContextName) and the context is simply skipped, so that one broken or
// unreachable context can never prevent rotation on the others.
func (w *Watcher) checkAndRotateContext(ctx context.Context, result docker.ContextClientResult) {
	contextLog := w.log.With(slog.String("context", result.DisplayName()))

	if result.Err != nil {
		contextLog.Error("skipping docker context for certificate rotation", logger.ErrAttr(result.Err))
		return
	}

	if result.Cli == nil {
		contextLog.Error("skipping docker context for certificate rotation: no docker client available")
		return
	}

	// A Swarm manager can also host ordinary Compose deployments. Scan both
	// resource kinds independently so rotation redeploys with the same mode
	// the project was deployed with.
	modes := []bool{false}
	if result.SwarmMode {
		modes = append(modes, true)
	}

	for _, swarmMode := range modes {
		w.checkAndRotateContextMode(ctx, result, contextLog, swarmMode)
	}
}

func (w *Watcher) checkAndRotateContextMode(ctx context.Context, result docker.ContextClientResult, contextLog *slog.Logger, swarmMode bool) {
	services, err := docker.GetLabeledServices(
		ctx, result.Cli.Client(), swarmMode,
		docker.DocoCDLabels.Deployment.CertRotatable, "true",
	)
	if err != nil {
		contextLog.Error("failed to list certificate-rotatable deployments",
			slog.Bool("swarm_mode", swarmMode), logger.ErrAttr(err))

		return
	}

	due := dueProjects(services, w.now(), w.threshold, contextLog)
	revoked := revokedProjects(ctx, services, w.secretProvider, contextLog)

	// Reasons must be computed before merging revoked into due, otherwise a project that is only
	// revoked (and not yet near expiry) would be reported as being due for "expiry" as well.
	reasons := rotationReasons(due, revoked)

	maps.Copy(due, revoked)

	for project, labels := range due {
		contextLog.Info("certificate needs rotation",
			slog.String("project", project),
			slog.String("reason", strings.Join(reasons[project], ",")),
			slog.Bool("swarm_mode", swarmMode),
		)

		if err := docker.RotateProjectCertificates(ctx, result.Name, result.Cli, labels, w.secretProvider, swarmMode, w.certOptions); err != nil {
			contextLog.Error("failed to rotate certificate",
				slog.String("project", project),
				slog.Bool("swarm_mode", swarmMode),
				logger.ErrAttr(err),
			)

			continue
		}

		contextLog.Info("certificate rotation redeploy completed",
			slog.String("project", project),
			slog.Bool("swarm_mode", swarmMode),
		)
	}
}

// projectKey returns the project/stack identifier used to group discovered services for
// rotation-due and revocation checks. Standalone Docker Compose deployments carry Compose's own
// api.ProjectLabel; Docker Swarm stacks never set that label (docker stack deploy doesn't apply
// it), so DocoCDLabels.Deployment.Name (set by doco-cd itself for both modes) is used as a
// fallback.
func projectKey(labels map[string]string) string {
	if project := labels[api.ProjectLabel]; project != "" {
		return project
	}

	return labels[docker.DocoCDLabels.Deployment.Name]
}

func revokedProjects(
	ctx context.Context,
	services map[docker.Service]map[string]string,
	provider secretprovider.SecretProvider,
	log *slog.Logger,
) map[string]map[string]string {
	if provider == nil {
		return nil
	}

	checker, ok := provider.(secretprovider.DeploymentCertificateRevocationChecker)
	if !ok {
		return nil
	}

	revoked := make(map[string]map[string]string)

	for _, labels := range services {
		project := projectKey(labels)
		if project == "" {
			continue
		}

		// One revoked certificate is enough to rotate the whole project, so skip the remaining
		// services of a project already known to be revoked instead of re-querying the provider.
		if _, alreadyRevoked := revoked[project]; alreadyRevoked {
			continue
		}

		certState := labels[docker.DocoCDLabels.Deployment.CertState]
		if certState == "" {
			continue
		}

		isRevoked, err := checker.DeploymentHasRevokedCertificate(ctx, certState)
		if err != nil {
			if log != nil {
				log.Warn("skipping deployment with unreadable cert revocation state",
					slog.String("project", project),
					logger.ErrAttr(err),
				)
			}

			continue
		}

		if isRevoked {
			revoked[project] = labels
		}
	}

	return revoked
}

func rotationReasons(
	expiryDue map[string]map[string]string,
	revokedDue map[string]map[string]string,
) map[string][]string {
	reasons := make(map[string][]string, len(expiryDue)+len(revokedDue))

	for project := range expiryDue {
		reasons[project] = append(reasons[project], "expiry")
	}

	for project := range revokedDue {
		reasons[project] = append(reasons[project], "revoked")
	}

	for project := range reasons {
		slices.Sort(reasons[project])
	}

	return reasons
}

// dueProjects deduplicates discovered rotation-capable services by compose project. If services
// have differing expiry labels (e.g. a partially failed rotation left one stale), it uses the
// soonest expiry so a near-expiry certificate can never be masked by a sibling's fresher label.
func dueProjects(
	services map[docker.Service]map[string]string,
	now time.Time,
	threshold time.Duration,
	log *slog.Logger,
) map[string]map[string]string {
	byProject := make(map[string]map[string]string, len(services))
	expiries := make(map[string]time.Time, len(services))

	for _, labels := range services {
		project := projectKey(labels)
		if project == "" {
			continue
		}

		expiry, ok := parseCertExpiry(labels[docker.DocoCDLabels.Deployment.CertExpiry])
		if !ok {
			if log != nil {
				log.Warn("skipping deployment with invalid or missing cert expiry label",
					slog.String("project", project))
			}

			continue
		}

		if current, seen := expiries[project]; !seen || expiry.Before(current) {
			byProject[project] = labels
			expiries[project] = expiry
		}
	}

	due := make(map[string]map[string]string, len(byProject))

	for project, labels := range byProject {
		expiry := expiries[project]
		if now.Add(threshold).Before(expiry) {
			// Not due yet.
			continue
		}

		due[project] = labels
	}

	return due
}

// parseCertExpiry parses the cd.doco.deployment.cert.expiry label value (RFC3339).
func parseCertExpiry(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}

	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}

	return t, true
}

// formatWatcherDuration formats a time.Duration for logging, using the largest whole unit (hours or minutes).
func formatWatcherDuration(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh", d/time.Hour)
	}

	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}

	return d.String()
}
