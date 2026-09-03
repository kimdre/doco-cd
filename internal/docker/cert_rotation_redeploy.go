package docker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// certRotationTrigger is the value stamped into DocoCDLabels.Deployment.Trigger by rotation-driven
// redeploys, distinguishing them from webhook, poll, or scheduled-job triggered redeploys.
const certRotationTrigger = "cert.rotation"

// RotateProjectCertificates reloads the deploy config for a rotatable deployment (identified by
// its doco-cd labels, see DocoCDLabels.Deployment.CertRotatable), re-resolves its external secrets
// (reissuing any pki-role certificates through the configured secret provider), and redeploys so
// the fresh values take effect.
//
// contextName identifies the Docker context dockerCli was created for (empty for the local
// default context, see NormalizeContextName/DisplayContextName); it is only used to namespace the
// per-stack scheduler/deploy lock (see lock.StackKey) so that same-named stacks on different
// Docker contexts don't block each other. Discovered resources need no extra "context" label of
// their own for this: the caller (certrotation.Watcher) already knows which context's client
// produced labels, since it scans one context's resources at a time.
//
// Compose deployments only recreate the services actually consuming a rotated certificate/key.
// Swarm stacks redeploy the whole stack, but Swarm's own spec diffing (see
// stableSwarmMetadataLabels) still limits recreation to the affected services.
func RotateProjectCertificates(
	ctx context.Context,
	contextName string,
	dockerCli command.Cli,
	labels map[string]string,
	secretProvider secretprovider.SecretProvider,
	swarmMode bool,
	opts CertificateRotationOptions,
) error {
	if swarmMode {
		return rotateSwarmProjectCertificates(ctx, contextName, dockerCli, labels, secretProvider, opts)
	}

	ref, err := composeScheduledServiceRefFromLabels(labels)
	if err != nil {
		return fmt.Errorf("parse deployment labels for cert rotation: %w", err)
	}

	stackName := strings.TrimSpace(labels[DocoCDLabels.Deployment.Name])
	if stackName == "" {
		stackName = ref.Project
	}

	stackLockKey := lock.StackKey(contextName, stackName)

	lock.LockStack(stackLockKey)
	defer lock.UnlockStack(stackLockKey)

	project, deployConfig, err := loadComposeScheduledProjectAll(ctx, dockerCli, ref, secretProvider, opts.Scheduled)
	if err != nil {
		return fmt.Errorf("reload deploy config for cert rotation of %s: %w", ref.Project, err)
	}

	// Only recreate the services that actually consume a pki-role-backed certificate or private
	// key, so unrelated services in the same project aren't disrupted by a rotation they have no
	// part in. If we can't determine any affected service (e.g. an unexpected project structure),
	// fall back to recreating every service rather than silently doing nothing after certificates
	// have already been reissued.
	serviceNames := servicesUsingRotatableCerts(project, deployConfig)
	if len(serviceNames) == 0 {
		serviceNames = make([]string, 0, len(project.Services))
		for name := range project.Services {
			serviceNames = append(serviceNames, name)
		}
	}

	selectedProject, err := project.WithSelectedServices(serviceNames, types.IgnoreDependencies)
	if err != nil {
		return fmt.Errorf("select certificate-consuming services for rotation of %s: %w", ref.Project, err)
	}

	payload := certRotationPayload(labels, resolvedSourceType(ref, opts.Scheduled))

	timestamp := time.Now().UTC().Format(time.RFC3339)
	latestCommit := strings.TrimSpace(labels[DocoCDLabels.Deployment.CommitSHA])
	projectHash := strings.TrimSpace(labels[DocoCDLabels.Deployment.ComposeHash])

	addComposeServiceLabels(selectedProject, deployConfig, payload, ref.WorkingDir, app.Version, timestamp, ComposeVersion, latestCommit, projectHash)

	if err = deployCompose(ctx, dockerCli, selectedProject, deployConfig, api.RecreateForce, serviceNames, nil, func(string) {}); err != nil {
		return fmt.Errorf("redeploy project %s for cert rotation: %w", ref.Project, err)
	}

	return nil
}

// rotateSwarmProjectCertificates reissues certificates for a Docker Swarm deployment and
// redeploys the whole stack. Unlike the standalone Compose path, no per-service selection is
// needed: Swarm only recreates the tasks of services whose spec actually changed, so only the
// services consuming the rotated certificate values end up being redeployed.
//
// contextName is used the same way as in RotateProjectCertificates: only to namespace the
// per-stack lock so the same stack name on different Docker contexts never blocks each other.
func rotateSwarmProjectCertificates(
	ctx context.Context,
	contextName string,
	dockerCli command.Cli,
	labels map[string]string,
	secretProvider secretprovider.SecretProvider,
	certOpts CertificateRotationOptions,
) error {
	ref, err := composeScheduledServiceRefFromSwarmLabels(labels)
	if err != nil {
		return fmt.Errorf("parse deployment labels for cert rotation: %w", err)
	}

	stackLockKey := lock.StackKey(contextName, ref.Project)

	lock.LockStack(stackLockKey)
	defer lock.UnlockStack(stackLockKey)

	project, deployConfig, err := loadComposeScheduledProjectAll(ctx, dockerCli, ref, secretProvider, certOpts.Scheduled)
	if err != nil {
		return fmt.Errorf("reload deploy config for cert rotation of %s: %w", ref.Project, err)
	}

	payload := certRotationPayload(labels, resolvedSourceType(ref, certOpts.Scheduled))

	timestamp := time.Now().UTC().Format(time.RFC3339)
	latestCommit := strings.TrimSpace(labels[DocoCDLabels.Deployment.CommitSHA])
	projectHash := strings.TrimSpace(labels[DocoCDLabels.Deployment.ComposeHash])

	cfg, opts, err := LoadSwarmStack(dockerCli, project, deployConfig, ref.WorkingDir)
	if err != nil {
		return fmt.Errorf("load swarm stack for cert rotation of %s: %w", ref.Project, err)
	}

	addSwarmServiceLabels(cfg, project, deployConfig, payload, ref.WorkingDir, app.Version, timestamp, latestCommit, projectHash)
	addSwarmVolumeLabels(cfg, deployConfig, payload, ref.WorkingDir)
	addSwarmConfigLabels(cfg, deployConfig, payload, ref.WorkingDir, app.Version, timestamp, latestCommit)
	addSwarmSecretLabels(cfg, deployConfig, payload, ref.WorkingDir, app.Version, timestamp, latestCommit)

	if err = removeMismatchedRecreatableVolumes(ctx, dockerCli.Client(), ref.Project, project); err != nil {
		return fmt.Errorf("remove mismatched recreatable volumes for cert rotation of %s: %w", ref.Project, err)
	}

	if err = DeploySwarmStack(ctx, dockerCli, cfg, opts); err != nil {
		return fmt.Errorf("redeploy swarm stack %s for cert rotation: %w", ref.Project, err)
	}

	pruneSwarmStackRevisions(ctx, dockerCli, ref.Project, deployConfig, certOpts)

	return nil
}

// pruneSwarmStackRevisions removes superseded config/secret revisions left behind by the
// rotation redeploy, honoring the same retention settings as a normal Swarm deploy. Prune
// failures are only logged, not returned, since the certificate has already been redeployed
// successfully by the time this runs.
func pruneSwarmStackRevisions(ctx context.Context, dockerCli command.Cli, stackName string, deployConfig *deploy.Config, opts CertificateRotationOptions) {
	if retention := deployConfig.ResolveSwarmConfigRetention(opts.SwarmRetention.Config); retention >= 0 {
		if err := PruneStackConfigs(ctx, dockerCli.Client(), stackName, retention); err != nil {
			slog.Warn("failed to prune swarm stack configs after cert rotation",
				slog.String("project", stackName), logger.ErrAttr(err))
		}
	}

	if retention := deployConfig.ResolveSwarmSecretRetention(opts.SwarmRetention.Secret); retention >= 0 {
		if err := PruneStackSecrets(ctx, dockerCli.Client(), stackName, retention); err != nil {
			slog.Warn("failed to prune swarm stack secrets after cert rotation",
				slog.String("project", stackName), logger.ErrAttr(err))
		}
	}
}

// resolvedSourceType reports the source type whose on-disk layout ref's repository actually
// matches, falling back to the labeled one when neither directory is present.
func resolvedSourceType(ref composeScheduledServiceRef, opts ScheduledComposeOptions) config.SourceType {
	_, sourceType, err := resolveScheduledSourceRepo(ref, opts.ComposeLoad.DataMountPath)
	if err != nil {
		return config.NormalizeSourceType(config.SourceType(ref.SourceType))
	}

	return sourceType
}

// certRotationPayload builds a synthetic payload for relabeling rotated services.
// Trigger identifies the rotation-driven redeploy, while CommitSHA remains unset.
// sourceType reflects the source that actually resolved rather than a potentially stale label,
// keeping recreated services consistent with the rest of the deployment.
func certRotationPayload(labels map[string]string, sourceType config.SourceType) *webhook.ParsedPayload {
	return &webhook.ParsedPayload{
		Source:   webhook.PayloadSource(SourceTypeLabelValue(string(sourceType), labels[DocoCDLabels.Source.Type])),
		Trigger:  certRotationTrigger,
		FullName: strings.TrimSpace(labels[DocoCDLabels.Source.Name]),
		WebURL:   strings.TrimSpace(labels[DocoCDLabels.Source.URL]),
	}
}
