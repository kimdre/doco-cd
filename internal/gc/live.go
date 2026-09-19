// Package gc implements artifact garbage collection:
// periodically removing published source artifacts (internal/source/store) that are no longer
// referenced by any deployed stack or service, are past their retention window,
// and are not currently being used by an in-progress deployment.
//
// This exists for two reasons: reclaiming disk space for repositories/OCI artifacts
// that accumulate one artifact directory per revision, and
// bounding how long a decrypted SOPS secret (Phase 2 decrypts once at
// publish time, into the artifact directory) can linger on disk after the
// revision that needed it stops being deployed.
//
// The live set - what "referenced" means - always wins over retention: a
// revision currently deployed anywhere, on any configured Docker context, or
// currently being prepared for a deployment that has not finished yet
// (see internal/source.IsInFlight), is never removed regardless of age or count.
// Sweep (internal/source/store) additionally never looks outside each
// repository's "artifacts/<revision>" entries, so the mirror clone,
// submodule cache, and any in-progress temporary publish directory can never
// be touched by construction.
//
// Sweeper is a singleton, ticker-driven component constructed once at
// startup (mirroring internal/certrotation.Watcher) - it must not be hung
// off store construction, which happens once per deployment and would
// otherwise turn every deployment into an implicit, overlapping GC run.
package gc

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/source/oci"
	"github.com/kimdre/doco-cd/internal/source/store"
)

const allRevisions store.Revision = "*"

// LiveRevisions discovers, across every configured Docker context, the set
// of source revisions currently referenced by a deployed stack or service,
// keyed by normalized repository/artifact name (docker.NormalizeRepositoryLabel).
//
// A repository/revision pair is included if the label pair is present on any
// container or swarm service - running or stopped. A stopped-but-still
// -deployed stack must keep its artifact just as much as a running one: it
// can be started again at any time without doco-cd re-preparing its source.
//
// Discovery fails closed: if any configured context cannot be inspected, no
// artifacts may be swept because that context could still reference them.
func LiveRevisions(
	ctx context.Context,
	contexts *docker.ContextRegistry,
	log *slog.Logger,
	dataMountSource string,
	dataMountDestination string,
) (map[string]set.Set[store.Revision], error) {
	live := make(map[string]set.Set[store.Revision])

	if contexts == nil {
		return live, nil
	}

	results, err := contexts.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list docker contexts: %w", err)
	}

	for _, result := range results {
		if result.Err != nil {
			return nil, fmt.Errorf("inspect docker context %s: %w", result.DisplayName(), result.Err)
		}

		if result.Cli == nil {
			return nil, fmt.Errorf("inspect docker context %s: missing client", result.DisplayName())
		}

		// A Swarm manager can also host ordinary Compose deployments, same
		// as internal/certrotation - scan both resource kinds independently.
		modes := []bool{false}
		if result.SwarmMode {
			modes = append(modes, true)
		}

		for _, swarmMode := range modes {
			if err := addLiveRevisions(ctx, result, swarmMode, live, log, dataMountSource, dataMountDestination); err != nil {
				return nil, err
			}
		}
	}

	return live, nil
}

// addLiveRevisions lists every doco-cd-managed container or service (any value of the source-name label)
// in one Docker context/mode and folds its (repository, revision) pair into live.
func addLiveRevisions(
	ctx context.Context,
	result docker.ContextClientResult,
	swarmMode bool,
	live map[string]set.Set[store.Revision],
	log *slog.Logger,
	dataMountSource string,
	dataMountDestination string,
) error {
	contextLog := log.With(slog.String("context", result.DisplayName()), slog.Bool("swarm_mode", swarmMode))

	services, err := docker.GetServicesWithLabelKey(ctx, result.Cli.Client(), swarmMode, docker.DocoCDLabels.Source.Name)
	if err != nil {
		contextLog.Error("gc: failed to list deployed sources", logger.ErrAttr(err))
		return fmt.Errorf("list deployed sources in context %s: %w", result.DisplayName(), err)
	}

	for _, labels := range services {
		get := docker.Labels(labels).Get

		repoName, ok := get(docker.DocoCDLabels.Source.Name)
		if !ok || repoName == "" {
			continue
		}

		revision, ok := get(docker.DocoCDLabels.Deployment.CommitSHA)
		if !ok || revision == "" {
			continue
		}

		normalized := repositoryFromWorkingDir(
			labels[docker.DocoCDLabels.Deployment.WorkingDir],
			store.Revision(revision),
			dataMountSource,
			dataMountDestination,
		)
		if normalized == "" {
			normalized = docker.NormalizeRepositoryLabel(repoName)
		}

		if normalized == "" {
			continue
		}

		if live[normalized] == nil {
			live[normalized] = make(set.Set[store.Revision])
		}

		live[normalized].Add(store.Revision(revision))

		configRevision, ok := get(docker.DocoCDLabels.Source.ConfigRevision)
		if !ok || configRevision == "" {
			configRepo := repositoryFromSourceLabels(labels)
			if configRepo != "" && configRepo != normalized {
				if live[configRepo] == nil {
					live[configRepo] = make(set.Set[store.Revision])
				}

				live[configRepo].Add(allRevisions)
			}

			continue
		}

		configRepo := repositoryFromWorkingDir(
			labels[docker.DocoCDLabels.Source.ConfigWorkingDir],
			store.Revision(configRevision),
			dataMountSource,
			dataMountDestination,
		)
		if configRepo == "" {
			continue
		}

		if live[configRepo] == nil {
			live[configRepo] = make(set.Set[store.Revision])
		}

		live[configRepo].Add(store.Revision(configRevision))
	}

	return nil
}

func repositoryFromSourceLabels(labels map[string]string) string {
	sourceURL := strings.TrimSpace(labels[docker.DocoCDLabels.Source.URL])
	sourceType := config.NormalizeSourceType(config.SourceType(labels[docker.DocoCDLabels.Source.Type]))

	if sourceURL != "" {
		if sourceType == config.SourceTypeOCI {
			return filepath.ToSlash(oci.RepositoryNameFromArtifact(sourceURL))
		}

		return filepath.ToSlash(git.GetRepoName(sourceURL))
	}

	return docker.NormalizeRepositoryLabel(labels[docker.DocoCDLabels.Source.Name])
}

func repositoryFromWorkingDir(workingDir string, revision store.Revision, dataMountRoots ...string) string {
	workingDir = strings.TrimSpace(workingDir)
	if workingDir == "" || revision == "" {
		return ""
	}

	revisionDir := store.ArtifactDirName(revision)

	for _, root := range dataMountRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}

		rel, err := filepath.Rel(root, workingDir)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}

		parts := strings.Split(filepath.ToSlash(rel), "/")
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == store.ArtifactsSubdir && parts[i+1] == revisionDir && i > 0 {
				return strings.Join(parts[:i], "/")
			}
		}
	}

	return ""
}
