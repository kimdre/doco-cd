package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/reconciliation"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// recoverStateWaitBudget bounds the total time startup state recovery may spend waiting for
// recovered jobs to report their event listeners ready. Exceeding it never drops a recovery:
// every job is already registered by then and finishes initializing in the background.
const recoverStateWaitBudget = 60 * time.Second

// recoverReconciliationState rebuilds in-memory reconciliation state (job registry, event
// listeners, unhealthy-restart suppression history) for repositories that were already
// deployed before this process started. It relies entirely on doco-cd labels already
// present on existing containers/services and on the Git/OCI checkout already present on
// the data volume to reload deploy configs, so that reload step requires no network access
// and never re-fetches/re-clones a source: repositories whose local checkout is no longer
// present are skipped (logged as a warning) and are left for the next real poll/webhook
// trigger to recover instead. Recovery then runs the same one-time startup healing a
// normal deploy would (restarting containers already unhealthy, redeploying stacks with no
// running containers/services at all), so drift accumulated while the process was down is
// corrected immediately instead of waiting for the next poll/webhook trigger. Unlike the
// config reload, that healing can reach the network exactly like any other reconciliation
// redeploy (git fetch/OCI pull) when it redeploys a missing stack.
func recoverReconciliationState(
	ctx context.Context,
	appConfig *app.Config,
	contexts *docker.ContextRegistry,
	manager *reconciliation.Manager,
	dataMountPoint container.MountPoint,
	log *slog.Logger,
) {
	if !appConfig.RecoverOnStartup {
		log.Debug("reconciliation state recovery on startup is disabled by configuration")
		return
	}

	refs, err := docker.DiscoverManagedDeploymentsAllContexts(ctx, contexts, log)
	if err != nil {
		log.Error("failed to discover managed deployments for reconciliation state recovery", logger.ErrAttr(err))
		return
	}

	// Recovered jobs are registered synchronously but keep initializing in the background
	// (their lifetime is detached from ctx), so this budget only bounds how long startup
	// waits for them. Without it, several repositories on a slow or unreachable Docker
	// context would each add RecoverJob's own timeout to the time before doco-cd starts
	// serving webhooks.
	waitCtx, cancelWait := context.WithTimeout(ctx, recoverStateWaitBudget)
	defer cancelWait()

	for _, ref := range refs {
		if ctx.Err() != nil {
			log.Debug("stopping reconciliation state recovery: application is shutting down", logger.ErrAttr(ctx.Err()))
			return
		}

		recoverManagedDeployment(waitCtx, appConfig, manager, dataMountPoint, ref, log)
	}
}

// recoverManagedDeployment reloads deploy configs for a single previously deployed
// repository from its existing local checkout (no clone/fetch/pull) and, if any were
// found, registers a reconciliation job for it.
func recoverManagedDeployment(
	ctx context.Context,
	appConfig *app.Config,
	manager *reconciliation.Manager,
	dataMountPoint container.MountPoint,
	ref docker.ManagedDeploymentRef,
	log *slog.Logger,
) {
	repoLog := log.With(slog.String("repository", ref.RepositoryName))

	source, ok := docker.ResolveManagedSourceDir(dataMountPoint.Destination, ref.RepositoryURL, ref.SourceType)
	if !ok {
		repoLog.Warn("skipping reconciliation state recovery: local checkout not found on data volume; will recover on the next poll/webhook trigger instead")
		return
	}

	deployConfigs := reloadManagedDeployConfigs(appConfig, dataMountPoint.Destination, source, ref, repoLog)
	if len(deployConfigs) == 0 {
		repoLog.Debug("no reloadable deploy configs found for reconciliation state recovery")
		return
	}

	reference, revision := firstManagedTargetLabels(ref.Targets)

	err := manager.RecoverJob(ctx, reconciliation.DeployRequest{
		Logger:     repoLog,
		Metadata:   notification.Metadata{Repository: source.Name},
		JobTrigger: stages.JobTriggerPoll,
		Repository: stages.RepositoryData{
			Source:       source.Type,
			SourceUrl:    ref.RepositoryURL,
			Name:         source.Name,
			PathInternal: source.Path,
			Revision:     revision,
			// The artifact passed trust-policy verification when it was originally deployed,
			// and the reconciliation deploy path refuses to run without this. If the trust
			// policy was tightened while doco-cd was down, the next poll cycle replaces this
			// recovered job with a freshly verified one.
			OCITrusted: true,
		},
		DeployConfigs: deployConfigs,
		Payload:       recoveredPayload(ref, source.Type, reference, revision),
	})

	switch {
	case errors.Is(err, context.Canceled):
		repoLog.Debug("reconciliation state recovery canceled during shutdown", logger.ErrAttr(err))
	case errors.Is(err, reconciliation.ErrRecoverJobNotReady), errors.Is(err, context.DeadlineExceeded):
		// The job is registered and keeps initializing in the background; do not block startup.
		repoLog.Warn("recovered reconciliation state on startup, job is still initializing",
			slog.Int("deploy_configs", len(deployConfigs)), logger.ErrAttr(err))
	case err != nil:
		repoLog.Error("failed to recover reconciliation state", logger.ErrAttr(err))
	default:
		repoLog.Info("recovered reconciliation state on startup", slog.Int("deploy_configs", len(deployConfigs)))
	}
}

// recoveredPayload rebuilds the webhook payload of the deployment that originally created the
// recovered job, so notifications and commit statuses of any reconciliation deployment it
// triggers report the same repository identity and revision as the original deployment did.
func recoveredPayload(ref docker.ManagedDeploymentRef, sourceType config.SourceType, reference, revision string) *webhook.ParsedPayload {
	payload := &webhook.ParsedPayload{
		Source:   webhook.PayloadSourceGit,
		Name:     filepath.Base(ref.RepositoryName),
		FullName: ref.RepositoryName,
		CloneURL: ref.RepositoryURL,
		WebURL:   ref.RepositoryURL,
		Ref:      reference,
		Trigger:  revision,
	}

	switch {
	case sourceType == config.SourceTypeOCI:
		payload.Source = webhook.PayloadSourceOCI
		payload.Artifact = ref.RepositoryURL
		payload.Digest = revision
	case plumbing.IsHash(revision):
		payload.CommitSHA = plumbing.NewHash(revision)
	default:
		// Neither a digest nor a usable commit SHA, so report no trigger revision at all.
		payload.Trigger = ""
	}

	return payload
}

// reloadManagedDeployConfigs reloads and merges the deploy configs for every distinct
// configuration target previously deployed for ref, retaining only deployments observed
// in Docker labels and deduplicating by context and config name.
func reloadManagedDeployConfigs(
	appConfig *app.Config,
	dataMountPath string,
	source docker.ManagedSource,
	ref docker.ManagedDeploymentRef,
	repoLog *slog.Logger,
) []*deploy.Config {
	var deployConfigs []*deploy.Config

	seen := make(map[string]struct{})

	for _, target := range ref.Targets {
		configs, err := deploy.GetConfigs(
			source.Path,
			appConfig.DeployConfigBaseDir,
			target.ConfigTarget,
			target.Reference,
			&deploy.GitOptions{LocalOnly: true},
		)
		if err != nil {
			repoLog.Warn("failed to reload deploy config for reconciliation state recovery",
				slog.String("config_target", target.ConfigTarget), logger.ErrAttr(err))

			continue
		}

		for _, cfg := range configs {
			if cfg.Name != target.DeploymentName ||
				docker.NormalizeContextName(cfg.Context) != docker.NormalizeContextName(target.Context) {
				continue
			}

			if !managedConfigMatchesLocalSource(dataMountPath, source, cfg, target.Revision) {
				repoLog.Warn("skipping reconciliation target whose deployed revision is not present in the local source",
					slog.String("deployment", target.DeploymentName),
					slog.String("config_target", target.ConfigTarget),
					slog.String("reference", target.Reference),
					slog.String("revision", target.Revision))

				continue
			}

			if source.Type == config.SourceTypeOCI && target.Reference != "" {
				cfg.Reference = target.Reference
			}

			key := docker.NormalizeContextName(cfg.Context) + "\x00" + cfg.Name
			if _, dup := seen[key]; dup {
				continue
			}

			seen[key] = struct{}{}
			cfg.Internal.ConfigTarget = target.ConfigTarget

			if hash, err := cfg.Hash(); err == nil {
				cfg.Internal.Hash = hash
			}

			deployConfigs = append(deployConfigs, cfg)
		}
	}

	return deployConfigs
}

// firstManagedTargetLabels returns the first non-empty reference and revision across targets,
// so a target that was deployed without one of those labels does not mask the value of a later one.
func firstManagedTargetLabels(targets []docker.ManagedDeploymentTarget) (reference, revision string) {
	for _, target := range targets {
		if reference == "" {
			reference = strings.TrimSpace(target.Reference)
		}

		if revision == "" {
			revision = strings.TrimSpace(target.Revision)
		}

		if reference != "" && revision != "" {
			break
		}
	}

	return reference, revision
}

// managedConfigMatchesLocalSource checks whether the revision a managed deployment target was
// deployed at is the one currently present in the local source checkout. Targets without a
// revision label are assumed to match.
func managedConfigMatchesLocalSource(
	dataMountPath string,
	source docker.ManagedSource,
	cfg *deploy.Config,
	revision string,
) bool {
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return true
	}

	// Auto-discovery configs pointing at another repository are deployed from that
	// repository's own checkout next to this source.
	if repositoryURL := strings.TrimSpace(string(cfg.RepositoryUrl)); repositoryURL != "" {
		source = docker.ManagedSource{
			Path: filepath.Join(filepath.Dir(source.Path), gitInternal.GetRepoName(repositoryURL)),
			Type: config.SourceTypeGit,
		}
	}

	if source.Type == config.SourceTypeOCI {
		cachedRevision, err := sourcecache.ReadRevision(dataMountPath, source.Path, source.Type)
		if err != nil {
			return false
		}

		return strings.TrimPrefix(strings.TrimSpace(cachedRevision), "sha256:") ==
			strings.TrimPrefix(revision, "sha256:")
	}

	matches, err := gitInternal.HeadMatchesCommit(source.Path, revision)

	return err == nil && matches
}
