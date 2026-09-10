package docker

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/logger"
)

// ManagedDeploymentTarget identifies one previously deployed configuration target (the
// deploy config resolved from a specific ".doco-cd(.<ConfigTarget>).y(a)ml" file) for a
// managed repository, together with the git/OCI reference it was deployed at.
type ManagedDeploymentTarget struct {
	DeploymentName string
	ConfigTarget   string
	Reference      string
	Revision       string
	// ConfigHash is the deploy config hash (DocoCDLabels.Deployment.ConfigHash) recorded at
	// deploy time. It lets recovery detect real config drift for this specific target (its
	// resolved config changed since it was deployed) without being tripped up by unrelated
	// commits elsewhere in the repository advancing the shared checkout's HEAD, e.g. in a
	// monorepo where many independently deployed targets share one git checkout.
	ConfigHash string
	Context    string
}

// ManagedDeploymentRef identifies a previously deployed repository/artifact, discovered
// from the doco-cd labels of its currently known containers (running or stopped, not yet
// removed) and Swarm services, without filtering by any specific repository.
type ManagedDeploymentRef struct {
	RepositoryName string // DocoCDLabels.Source.Name label value (e.g. "owner/repo")
	RepositoryURL  string // DocoCDLabels.Source.URL label value
	SourceType     string // DocoCDLabels.Source.Type label value ("git" or "oci")
	Targets        []ManagedDeploymentTarget
}

// ManagedSource describes where a previously prepared source was extracted on the data volume.
type ManagedSource struct {
	Name string            // Data-volume-relative directory name (e.g. "github.com/owner/repo")
	Path string            // Absolute path of that directory inside the container
	Type config.SourceType // Source type the directory was resolved for
}

// managedDeploymentIndex groups discovered deployments by source, deduplicating both sources
// and their targets while preserving discovery order.
type managedDeploymentIndex struct {
	bySource map[string]*ManagedDeploymentRef
	order    []string
}

func newManagedDeploymentIndex() *managedDeploymentIndex {
	return &managedDeploymentIndex{bySource: make(map[string]*ManagedDeploymentRef)}
}

// ref returns the entry for a source, creating it on first sight. Sources are keyed by their
// normalized data-volume directory name, so the same repository name served from different
// hosts (or as a different source type) stays separate.
func (i *managedDeploymentIndex) ref(repositoryName, repositoryURL, sourceType string) *ManagedDeploymentRef {
	key := string(config.NormalizeSourceType(config.SourceType(sourceType))) + "\x00" +
		ManagedSourceName(repositoryURL, sourceType)
	if repositoryURL == "" {
		key += "\x00" + repositoryName
	}

	if ref, ok := i.bySource[key]; ok {
		return ref
	}

	ref := &ManagedDeploymentRef{
		RepositoryName: repositoryName,
		RepositoryURL:  repositoryURL,
		SourceType:     sourceType,
	}

	i.bySource[key] = ref
	i.order = append(i.order, key)

	return ref
}

// add records the deployment described by the doco-cd labels of one container or Swarm
// service running on contextName. Labels without a source/deployment identity are ignored.
func (i *managedDeploymentIndex) add(contextName string, labels map[string]string) {
	repositoryName := strings.TrimSpace(labels[DocoCDLabels.Source.Name])

	deploymentName := strings.TrimSpace(labels[DocoCDLabels.Deployment.Name])
	if repositoryName == "" || deploymentName == "" {
		return
	}

	sourceType := strings.TrimSpace(labels[DocoCDLabels.Source.Type])
	if sourceType == "" {
		sourceType = string(config.SourceTypeGit)
	}

	ref := i.ref(repositoryName, strings.TrimSpace(labels[DocoCDLabels.Source.URL]), sourceType)

	target := ManagedDeploymentTarget{
		DeploymentName: deploymentName,
		ConfigTarget:   strings.TrimSpace(labels[DocoCDLabels.Deployment.ConfigTarget]),
		Reference:      strings.TrimSpace(labels[DocoCDLabels.Deployment.TargetRef]),
		Revision:       strings.TrimSpace(labels[DocoCDLabels.Deployment.CommitSHA]),
		ConfigHash:     strings.TrimSpace(labels[DocoCDLabels.Deployment.ConfigHash]),
		Context:        contextName,
	}

	if !slices.Contains(ref.Targets, target) {
		ref.Targets = append(ref.Targets, target)
	}
}

// addContext indexes every container (running or stopped) and, when swarmMode is true, every
// Swarm service carrying doco-cd's manager label on the daemon behind apiClient.
func (i *managedDeploymentIndex) addContext(ctx context.Context, apiClient client.APIClient, contextName string, swarmMode bool) error {
	filters := make(client.Filters)
	filters.Add("label", DocoCDLabels.Metadata.Manager+"="+app.Name)

	containerResult, err := apiClient.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return err
	}

	for _, c := range containerResult.Items {
		i.add(contextName, c.Labels)
	}

	if !swarmMode {
		return nil
	}

	serviceResult, err := apiClient.ServiceList(ctx, client.ServiceListOptions{Filters: filters})
	if err != nil {
		return err
	}

	for _, s := range serviceResult.Items {
		i.add(contextName, s.Spec.Labels)
	}

	return nil
}

func (i *managedDeploymentIndex) refs() []ManagedDeploymentRef {
	refs := make([]ManagedDeploymentRef, 0, len(i.order))
	for _, key := range i.order {
		refs = append(refs, *i.bySource[key])
	}

	return refs
}

// DiscoverManagedDeployments groups every doco-cd managed container (running or stopped) and,
// when swarmMode is true, every managed Swarm service of a single Docker context by
// repository/artifact. Unlike ListManagedRepositoryContainers/ListManagedRepositoryServices it
// is not limited to one repository, so it can discover everything deployed by a previous
// process, e.g. to rebuild reconciliation state on startup.
func DiscoverManagedDeployments(ctx context.Context, apiClient client.APIClient, contextName string, swarmMode bool) ([]ManagedDeploymentRef, error) {
	index := newManagedDeploymentIndex()
	if err := index.addContext(ctx, apiClient, contextName, swarmMode); err != nil {
		return nil, err
	}

	return index.refs(), nil
}

// DiscoverManagedDeploymentsAllContexts runs DiscoverManagedDeployments against every Docker
// context of registry and merges the results, so a repository deployed to several contexts
// yields a single ref carrying one target per context. Contexts that cannot be reached or
// listed are logged and skipped instead of failing the whole discovery.
func DiscoverManagedDeploymentsAllContexts(ctx context.Context, registry *ContextRegistry, log *slog.Logger) ([]ManagedDeploymentRef, error) {
	clients, err := registry.List(ctx)
	if err != nil {
		return nil, err
	}

	index := newManagedDeploymentIndex()

	for _, contextClient := range clients {
		contextLog := log.With(slog.String("context", DisplayContextName(contextClient.Name)))

		if contextClient.Err != nil {
			contextLog.Warn("skipping docker context for managed deployment discovery", logger.ErrAttr(contextClient.Err))
			continue
		}

		if err = index.addContext(ctx, contextClient.Cli.Client(), contextClient.Name, contextClient.SwarmMode); err != nil {
			contextLog.Error("failed to discover managed deployments for docker context", logger.ErrAttr(err))
		}
	}

	return index.refs(), nil
}

// ManagedSourceName returns the normalized data-volume directory name for a source.
func ManagedSourceName(repositoryURL, sourceType string) string {
	return scheduledSourceRepoName(
		repositoryURL,
		config.NormalizeSourceType(config.SourceType(sourceType)),
	)
}

// ResolveManagedSourceDir locates the on-disk directory under dataMountPath where the source
// identified by repositoryURL was previously extracted (a Git checkout or an OCI artifact),
// trying the labeled source type first and falling back to the other scheme to support legacy
// or mislabeled deployments (mirroring resolveScheduledSourceRepo). ok is false when neither
// directory exists on disk, e.g. the data volume no longer has the checkout from a prior run.
func ResolveManagedSourceDir(dataMountPath, repositoryURL, labeledSourceType string) (source ManagedSource, ok bool) {
	if strings.TrimSpace(repositoryURL) == "" {
		return ManagedSource{}, false
	}

	labeled := config.NormalizeSourceType(config.SourceType(labeledSourceType))

	other := config.SourceTypeOCI
	if labeled == config.SourceTypeOCI {
		other = config.SourceTypeGit
	}

	for _, sourceType := range []config.SourceType{labeled, other} {
		name := ManagedSourceName(repositoryURL, string(sourceType))
		if name == "" {
			continue
		}

		path, err := filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPath, name), dataMountPath)
		if err != nil || !filesystem.IsDir(path) {
			continue
		}

		return ManagedSource{Name: filepath.ToSlash(name), Path: path, Type: sourceType}, true
	}

	return ManagedSource{}, false
}
