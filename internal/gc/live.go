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

// UsageKind tells why a deployment keeps a revision of a repository.
type UsageKind string

const (
	// UsageTarget is the revision the deployment was deployed from.
	UsageTarget UsageKind = "target"
	// UsagePinned is the revision of an older artifact that an unchanged service still mounts.
	UsagePinned UsageKind = "pinned"
	// UsageConfig is the revision of the repository that holds the deploy config of the deployment.
	UsageConfig UsageKind = "config"
)

// Usage is one reference from a deployed container or Swarm service to a revision of a repository.
type Usage struct {
	// Context is the display name of the Docker context that runs the deployment.
	Context string
	// Stack is the name of the deployment (cd.doco.deployment.name).
	// It falls back to the container or service name if the label is missing.
	Stack string
	// Revision is the referenced revision. It is "*" if the deployment has no config
	// revision metadata and so can use any revision of its config repository.
	Revision store.Revision
	Kind     UsageKind
}

// String returns a short description of the usage for log messages.
func (u Usage) String() string {
	return fmt.Sprintf("%s/%s@%s (%s)", u.Context, u.Stack, u.Revision, u.Kind)
}

// LiveRevisions discovers, across every configured Docker context, the set
// of source revisions currently referenced by a deployed stack or service,
// keyed by normalized repository/artifact name (docker.NormalizeRepositoryLabel).
//
// A repository/revision pair is included if the label pair is present on any
// container or swarm service - running or stopped. Revisions listed in the pinned
// revisions label are included as well: they are still mounted by services whose
// repository files did not change since. A stopped-but-still
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
	usages, err := LiveUsages(ctx, contexts, log, dataMountSource, dataMountDestination)
	if err != nil {
		return nil, err
	}

	return revisionsFromUsages(usages), nil
}

// LiveUsages discovers the same references as LiveRevisions, but also keeps who uses each
// revision: the Docker context, the stack and the kind of reference. The result is keyed
// by normalized repository/artifact name, the same as LiveRevisions.
//
// Discovery fails closed in the same way as LiveRevisions.
func LiveUsages(
	ctx context.Context,
	contexts *docker.ContextRegistry,
	log *slog.Logger,
	dataMountSource string,
	dataMountDestination string,
) (map[string][]Usage, error) {
	usages := make(map[string][]Usage)

	if contexts == nil {
		return usages, nil
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
			if err := addLiveUsages(ctx, result, swarmMode, usages, log, dataMountSource, dataMountDestination); err != nil {
				return nil, err
			}
		}
	}

	return usages, nil
}

// revisionsFromUsages folds usages into the revision sets that LiveRevisions returns.
func revisionsFromUsages(usages map[string][]Usage) map[string]set.Set[store.Revision] {
	live := make(map[string]set.Set[store.Revision], len(usages))

	for repoName, repoUsages := range usages {
		if live[repoName] == nil {
			live[repoName] = make(set.Set[store.Revision])
		}

		for _, usage := range repoUsages {
			live[repoName].Add(usage.Revision)
		}
	}

	return live
}

// addLiveUsages lists every doco-cd-managed container or service (any value of the source-name label)
// in one Docker context/mode and adds its (repository, revision) references to usages.
func addLiveUsages(
	ctx context.Context,
	result docker.ContextClientResult,
	swarmMode bool,
	usages map[string][]Usage,
	log *slog.Logger,
	dataMountSource string,
	dataMountDestination string,
) error {
	contextName := result.DisplayName()
	contextLog := log.With(slog.String("context", contextName), slog.Bool("swarm_mode", swarmMode))

	services, err := docker.GetServicesWithLabelKey(ctx, result.Cli.Client(), swarmMode, docker.DocoCDLabels.Source.Name)
	if err != nil {
		contextLog.Error("gc: failed to list deployed sources", logger.ErrAttr(err))
		return fmt.Errorf("list deployed sources in context %s: %w", contextName, err)
	}

	for serviceName, labels := range services {
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

		stack := strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.Name])
		if stack == "" {
			stack = strings.TrimPrefix(string(serviceName), "/")
		}

		add := func(repo string, revision store.Revision, kind UsageKind) {
			usages[repo] = append(usages[repo], Usage{Context: contextName, Stack: stack, Revision: revision, Kind: kind})
		}

		add(normalized, store.Revision(revision), UsageTarget)

		// Services whose repository files were unchanged by later deployments keep mounting the
		// artifact of the revision they were created from, see docker.DocoCDLabels.Deployment.PinnedRevisions.
		for _, pinned := range docker.ParsePinnedRevisions(labels[docker.DocoCDLabels.Deployment.PinnedRevisions]) {
			add(normalized, store.Revision(pinned), UsagePinned)
		}

		configRevision, ok := get(docker.DocoCDLabels.Source.ConfigRevision)
		if !ok || configRevision == "" {
			configRepo := repositoryFromSourceLabels(labels)
			if configRepo != "" && configRepo != normalized {
				add(configRepo, allRevisions, UsageConfig)
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

		add(configRepo, store.Revision(configRevision), UsageConfig)
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
