package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

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

// RecoverReconciliationState rebuilds in-memory reconciliation state (job registry, event
// listeners, unhealthy-restart suppression history) for repositories that were already
// deployed before this process started. It relies entirely on doco-cd labels already
// present on existing containers/services and on the Git/OCI checkout already present on
// the data volume, so it requires no network access and never re-fetches/re-clones a
// source: repositories whose local checkout is no longer present are skipped (logged as a
// warning) and are left for the next real poll/webhook trigger to recover instead. Recovery
// registers event listeners only; it does not run deployment startup-healing actions such as
// restarting unhealthy containers or redeploying missing services. Those still run when the
// next poll/webhook trigger replaces the recovered job with a fully deployed one.
//
// Without this, reconciliation state stays empty after a restart until the next poll cycle
// or webhook delivery triggers a deploy for each repository (see issue: "Recover
// reconciliation state on app re-/start").
func RecoverReconciliationState(
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

	clients, err := contexts.List(ctx)
	if err != nil {
		log.Error("failed to list docker contexts for reconciliation state recovery", logger.ErrAttr(err))
		return
	}

	refsBySource := make(map[string]*docker.ManagedDeploymentRef)
	order := make([]string, 0)

	for _, client := range clients {
		if client.Err != nil {
			log.Warn("skipping docker context for reconciliation state recovery",
				slog.String("context", docker.DisplayContextName(client.Name)), logger.ErrAttr(client.Err))

			continue
		}

		refs, err := docker.DiscoverManagedDeployments(ctx, client.Cli.Client(), client.SwarmMode)
		if err != nil {
			log.Error("failed to discover managed deployments for reconciliation state recovery",
				slog.String("context", docker.DisplayContextName(client.Name)), logger.ErrAttr(err))

			continue
		}

		for _, discovered := range refs {
			key := string(config.NormalizeSourceType(config.SourceType(discovered.SourceType))) + "\x00" +
				docker.ManagedSourceName(discovered.RepositoryURL, discovered.SourceType)
			if strings.TrimSpace(discovered.RepositoryURL) == "" {
				key += "\x00" + strings.TrimSpace(discovered.RepositoryName)
			}

			ref, exists := refsBySource[key]
			if !exists {
				ref = &docker.ManagedDeploymentRef{
					RepositoryName: discovered.RepositoryName,
					RepositoryURL:  discovered.RepositoryURL,
					SourceType:     discovered.SourceType,
				}
				refsBySource[key] = ref
				order = append(order, key)
			}

			for _, target := range discovered.Targets {
				target.Context = client.Name
				if !containsManagedTarget(ref.Targets, target) {
					ref.Targets = append(ref.Targets, target)
				}
			}
		}
	}

	for _, key := range order {
		recoverManagedDeployment(ctx, appConfig, manager, dataMountPoint, *refsBySource[key], log)
	}
}

func containsManagedTarget(targets []docker.ManagedDeploymentTarget, target docker.ManagedDeploymentTarget) bool {
	return slices.Contains(targets, target)
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
	repoLog := log.With(
		slog.String("repository", ref.RepositoryName),
	)

	sourceRepoPath, sourceType, ok := docker.ResolveManagedSourceDir(dataMountPoint.Destination, ref.RepositoryURL, ref.SourceType)
	if !ok {
		repoLog.Warn("skipping reconciliation state recovery: local checkout not found on data volume; will recover on the next poll/webhook trigger instead")
		return
	}

	deployConfigs := reloadManagedDeployConfigs(
		appConfig,
		dataMountPoint.Destination,
		sourceRepoPath,
		sourceType,
		ref,
		repoLog,
	)
	if len(deployConfigs) == 0 {
		repoLog.Debug("no reloadable deploy configs found for reconciliation state recovery")
		return
	}

	repositoryName, err := filepath.Rel(dataMountPoint.Destination, sourceRepoPath)
	if err != nil || repositoryName == "." || strings.HasPrefix(repositoryName, "..") {
		repoLog.Error("failed to derive local repository identity for reconciliation state recovery",
			slog.String("source_path", sourceRepoPath), logger.ErrAttr(err))

		return
	}

	repositoryName = filepath.ToSlash(repositoryName)

	payloadSource := webhook.PayloadSourceGit
	if sourceType == config.SourceTypeOCI {
		payloadSource = webhook.PayloadSourceOCI
	}

	payload := &webhook.ParsedPayload{
		Source:   payloadSource,
		Name:     filepath.Base(ref.RepositoryName),
		FullName: ref.RepositoryName,
		CloneURL: ref.RepositoryURL,
		WebURL:   ref.RepositoryURL,
		Ref:      firstManagedReference(ref.Targets),
	}

	revision := firstManagedRevision(ref.Targets)

	switch {
	case sourceType == config.SourceTypeOCI:
		payload.Artifact = ref.RepositoryURL
		payload.Digest = revision
		payload.Trigger = revision
	case plumbing.IsHash(revision):
		// Keep the deployed commit on the payload so notifications and commit statuses for
		// reconciliation deployments triggered by this recovered job report the right revision.
		payload.CommitSHA = plumbing.NewHash(revision)
		payload.Trigger = revision
	}

	err = manager.RecoverJob(ctx, reconciliation.DeployRequest{
		Logger:     repoLog,
		Metadata:   notification.Metadata{Repository: repositoryName},
		JobTrigger: stages.JobTriggerPoll,
		Repository: stages.RepositoryData{
			Source:       config.NormalizeSourceType(sourceType),
			SourceUrl:    ref.RepositoryURL,
			Name:         repositoryName,
			PathInternal: sourceRepoPath,
			Revision:     revision,
			OCITrusted:   true, // already deployed and trust-verified in a prior process lifetime
		},
		DeployConfigs: deployConfigs,
		Payload:       payload,
	})

	switch {
	case errors.Is(err, reconciliation.ErrRecoverJobNotReady):
		// The job is registered and keeps initializing in the background; do not block startup.
		repoLog.Warn("reconciliation state recovery is still initializing", logger.ErrAttr(err))
	case err != nil:
		repoLog.Error("failed to recover reconciliation state", logger.ErrAttr(err))

		return
	}

	repoLog.Info("recovered reconciliation state on startup", slog.Int("deploy_configs", len(deployConfigs)))
}

// reloadManagedDeployConfigs reloads and merges the deploy configs for every distinct
// configuration target previously deployed for ref, retaining only deployments observed
// in Docker labels and deduplicating by context and config name.
func reloadManagedDeployConfigs(
	appConfig *app.Config,
	dataMountPath string,
	sourceRepoPath string,
	sourceType config.SourceType,
	ref docker.ManagedDeploymentRef,
	repoLog *slog.Logger,
) []*deploy.Config {
	var deployConfigs []*deploy.Config

	seen := make(map[string]struct{})

	for _, target := range ref.Targets {
		configs, err := deploy.GetConfigs(
			sourceRepoPath,
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

			if !managedConfigMatchesLocalSource(dataMountPath, sourceRepoPath, sourceType, cfg, target) {
				repoLog.Warn("skipping reconciliation target whose deployed revision is not present in the local source",
					slog.String("deployment", target.DeploymentName),
					slog.String("config_target", target.ConfigTarget),
					slog.String("reference", target.Reference),
					slog.String("revision", target.Revision))

				continue
			}

			if sourceType == config.SourceTypeOCI && target.Reference != "" {
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

func firstManagedRevision(targets []docker.ManagedDeploymentTarget) string {
	for _, target := range targets {
		if revision := strings.TrimSpace(target.Revision); revision != "" {
			return revision
		}
	}

	return ""
}

// firstManagedReference returns the first non-empty git/OCI reference across targets, so a
// target that was deployed without a reference label does not mask the reference of a later one.
func firstManagedReference(targets []docker.ManagedDeploymentTarget) string {
	for _, target := range targets {
		if reference := strings.TrimSpace(target.Reference); reference != "" {
			return reference
		}
	}

	return ""
}

// managedConfigMatchesLocalSource checks whether the deployed revision for a managed
// deployment target is present in the local source checkout. If the target has no revision
// label, it is assumed to match.
func managedConfigMatchesLocalSource(
	dataMountPath string,
	sourceRepoPath string,
	sourceType config.SourceType,
	cfg *deploy.Config,
	target docker.ManagedDeploymentTarget,
) bool {
	revision := strings.TrimSpace(target.Revision)
	if revision == "" {
		return true
	}

	if strings.TrimSpace(string(cfg.RepositoryUrl)) != "" {
		sourceRepoPath = filepath.Join(filepath.Dir(sourceRepoPath), gitInternal.GetRepoName(string(cfg.RepositoryUrl)))
		sourceType = config.SourceTypeGit
	}

	normalizedSourceType := config.NormalizeSourceType(sourceType)
	if normalizedSourceType == config.SourceTypeOCI {
		cachedRevision, err := sourcecache.ReadRevision(dataMountPath, sourceRepoPath, normalizedSourceType)
		if err != nil {
			return false
		}

		return strings.TrimPrefix(strings.TrimSpace(cachedRevision), "sha256:") ==
			strings.TrimPrefix(revision, "sha256:")
	}

	matches, err := gitInternal.HeadMatchesCommit(sourceRepoPath, revision)

	return err == nil && matches
}
