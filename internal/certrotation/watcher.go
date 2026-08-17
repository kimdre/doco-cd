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

	// now is overridable in tests.
	now func() time.Time
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
		slog.Duration("threshold", w.threshold),
		slog.Duration("check_interval", w.checkInterval),
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

// checkAndRotate discovers all rotatable deployments, and for each project whose certificate
// expiry is within the configured threshold, triggers a rotation redeploy.
func (w *Watcher) checkAndRotate(ctx context.Context) {
	services, err := docker.GetLabeledServices(
		ctx, w.dockerCli.Client(), swarm.GetModeEnabled(),
		docker.DocoCDLabels.Deployment.CertRotatable, "true",
	)
	if err != nil {
		w.log.Error("failed to list certificate-rotatable deployments", logger.ErrAttr(err))
		return
	}

	due := dueProjects(services, w.now(), w.threshold, w.log)
	revoked := revokedProjects(ctx, services, w.secretProvider, w.log)
	maps.Copy(due, revoked)
	reasons := rotationReasons(due, revoked)

	for project, labels := range due {
		w.log.Info("certificate needs rotation",
			slog.String("project", project),
			slog.String("reason", strings.Join(reasons[project], ",")),
		)

		if err := docker.RotateProjectCertificates(ctx, w.dockerCli, labels, w.secretProvider, swarm.GetModeEnabled()); err != nil {
			w.log.Error("failed to rotate certificate",
				slog.String("project", project),
				logger.ErrAttr(err),
			)

			continue
		}

		w.log.Info("certificate rotation redeploy completed", slog.String("project", project))
	}
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
		project := labels[api.ProjectLabel]
		if project == "" {
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
// have differing expiry labels (for example, after a prior scoped rotation), it uses the latest
// expiry and its labels so an older stale label cannot cause repeated rotations.
func dueProjects(
	services map[docker.Service]map[string]string,
	now time.Time,
	threshold time.Duration,
	log *slog.Logger,
) map[string]map[string]string {
	byProject := make(map[string]map[string]string, len(services))
	expiries := make(map[string]time.Time, len(services))

	for _, labels := range services {
		project := labels[api.ProjectLabel]
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

		if current, seen := expiries[project]; !seen || expiry.After(current) {
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
