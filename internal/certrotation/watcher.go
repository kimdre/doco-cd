// Package certrotation implements a background watcher that automatically rotates certificates
// issued through rotation-capable external secret providers (currently only OpenBao PKI roles,
// see internal/secretprovider/openbao) before they expire.
//
// Deployments become eligible for automatic rotation when all of their certificate-bearing
// external secrets use a rotation-capable reference (e.g. OpenBao's "pki-role:" ref) — this is
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

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

// Watcher periodically checks all deployments carrying rotation-capable certificate labels and
// triggers reissuance + redeploy for any deployment whose certificate expiry falls within the
// configured rotation threshold.
type Watcher struct {
	dockerCli      command.Cli
	log            *slog.Logger
	secretProvider *secretprovider.SecretProvider
	threshold      time.Duration
	checkInterval  time.Duration

	// now and swarmEnabled are overridable in tests.
	now          func() time.Time
	swarmEnabled func() bool
}

// New creates a Watcher. threshold is how far ahead of a certificate's expiry rotation is
// triggered; checkInterval is how often deployments are checked.
func New(
	dockerCli command.Cli,
	log *slog.Logger,
	secretProvider *secretprovider.SecretProvider,
	threshold, checkInterval time.Duration,
) *Watcher {
	return &Watcher{
		dockerCli:      dockerCli,
		log:            log.With(slog.String("component", "certrotation")),
		secretProvider: secretProvider,
		threshold:      threshold,
		checkInterval:  checkInterval,
		now:            time.Now,
		swarmEnabled:   swarm.GetModeEnabled,
	}
}

// Start runs the watcher loop until ctx is cancelled. It performs an initial check immediately,
// then rechecks every checkInterval. Callers are expected to run Start in its own goroutine, e.g.
// via graceful.SafeGo, which handles WaitGroup bookkeeping.
func (w *Watcher) Start(ctx context.Context) {
	if w.dockerCli == nil || w.log == nil {
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

// swarmMode reports whether the Docker host is running in Swarm mode, via an overridable hook so
// tests can exercise both modes without a live daemon.
func (w *Watcher) swarmMode() bool {
	if w.swarmEnabled == nil {
		return swarm.GetModeEnabled()
	}

	return w.swarmEnabled()
}

// checkAndRotate discovers all rotatable deployments, and for each project whose certificate
// expiry is within the configured threshold, triggers a rotation redeploy.
func (w *Watcher) checkAndRotate(ctx context.Context) {
	services, err := docker.GetLabeledServices(
		ctx, w.dockerCli.Client(), w.swarmMode(),
		docker.DocoCDLabels.Deployment.CertRotatable, "true",
	)
	if err != nil {
		w.log.Error("failed to list certificate-rotatable deployments", logger.ErrAttr(err))
		return
	}

	due := dueProjects(services, w.now(), w.threshold, w.log)
	revoked := revokedProjects(ctx, services, w.secretProvider, w.log)

	// Reasons must be computed before merging revoked into due, otherwise a project that is only
	// revoked (and not yet near expiry) would be reported as being due for "expiry" as well.
	reasons := rotationReasons(due, revoked)

	maps.Copy(due, revoked)

	for project, labels := range due {
		w.log.Info("certificate needs rotation",
			slog.String("project", project),
			slog.String("reason", strings.Join(reasons[project], ",")),
		)

		if err := docker.RotateProjectCertificates(ctx, w.dockerCli, labels, w.secretProvider, w.swarmMode()); err != nil {
			w.log.Error("failed to rotate certificate",
				slog.String("project", project),
				logger.ErrAttr(err),
			)

			continue
		}

		w.log.Info("certificate rotation redeploy completed", slog.String("project", project))
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
	provider *secretprovider.SecretProvider,
	log *slog.Logger,
) map[string]map[string]string {
	if provider == nil || *provider == nil {
		return nil
	}

	checker, ok := (*provider).(secretprovider.DeploymentCertificateRevocationChecker)
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
