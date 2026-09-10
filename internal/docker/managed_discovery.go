package docker

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

// ManagedDeploymentTarget identifies one previously deployed configuration target (the
// deploy config resolved from a specific ".doco-cd(.<ConfigTarget>).y(a)ml" file) for a
// managed repository, together with the git/OCI reference it was deployed at.
type ManagedDeploymentTarget struct {
	DeploymentName string
	ConfigTarget   string
	Reference      string
	Revision       string
	Context        string
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

// DiscoverManagedDeployments lists every container (running or stopped) and, when
// swarmMode is true, every Swarm service carrying doco-cd's manager label, and groups them
// by repository/artifact. Unlike ListManagedRepositoryContainers/ListManagedRepositoryServices,
// it is not filtered to a single repository: it is meant to discover every repository that
// was already deployed before the current process started, e.g. to rebuild reconciliation
// state on startup.
func DiscoverManagedDeployments(ctx context.Context, apiClient client.APIClient, swarmMode bool) ([]ManagedDeploymentRef, error) {
	filters := make(client.Filters)
	filters.Add("label", DocoCDLabels.Metadata.Manager+"="+app.Name)

	byRepo := make(map[string]*ManagedDeploymentRef)
	order := make([]string, 0)

	addEntry := func(labels map[string]string) {
		repoName := strings.TrimSpace(labels[DocoCDLabels.Source.Name])

		deploymentName := strings.TrimSpace(labels[DocoCDLabels.Deployment.Name])
		if repoName == "" || deploymentName == "" {
			return
		}

		repositoryURL := strings.TrimSpace(labels[DocoCDLabels.Source.URL])

		sourceType := strings.TrimSpace(labels[DocoCDLabels.Source.Type])
		if sourceType == "" {
			sourceType = string(config.SourceTypeGit)
		}

		sourceKey := string(config.NormalizeSourceType(config.SourceType(sourceType))) + "\x00" +
			ManagedSourceName(repositoryURL, sourceType)
		if repositoryURL == "" {
			sourceKey += "\x00" + repoName
		}

		ref, ok := byRepo[sourceKey]
		if !ok {
			ref = &ManagedDeploymentRef{
				RepositoryName: repoName,
				RepositoryURL:  repositoryURL,
				SourceType:     sourceType,
			}
			byRepo[sourceKey] = ref
			order = append(order, sourceKey)
		}

		if ref.RepositoryURL == "" {
			ref.RepositoryURL = repositoryURL
		}

		if ref.SourceType == "" {
			ref.SourceType = sourceType
		}

		target := ManagedDeploymentTarget{
			DeploymentName: deploymentName,
			ConfigTarget:   strings.TrimSpace(labels[DocoCDLabels.Deployment.ConfigTarget]),
			Reference:      strings.TrimSpace(labels[DocoCDLabels.Deployment.TargetRef]),
			Revision:       strings.TrimSpace(labels[DocoCDLabels.Deployment.CommitSHA]),
		}

		if slices.Contains(ref.Targets, target) {
			return
		}

		ref.Targets = append(ref.Targets, target)
	}

	containerResult, err := apiClient.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, err
	}

	for _, c := range containerResult.Items {
		addEntry(c.Labels)
	}

	if swarmMode {
		serviceResult, err := apiClient.ServiceList(ctx, client.ServiceListOptions{Filters: filters})
		if err != nil {
			return nil, err
		}

		for _, s := range serviceResult.Items {
			addEntry(s.Spec.Labels)
		}
	}

	refs := make([]ManagedDeploymentRef, 0, len(order))
	for _, key := range order {
		refs = append(refs, *byRepo[key])
	}

	return refs, nil
}

// ManagedSourceName returns the normalized data-volume directory name for a source.
func ManagedSourceName(repositoryURL, sourceType string) string {
	return scheduledSourceRepoName(
		repositoryURL,
		config.NormalizeSourceType(config.SourceType(sourceType)),
	)
}

// ResolveManagedSourceDir locates the on-disk directory under dataMountPath where the
// source identified by repositoryURL was previously extracted (a Git checkout or an OCI
// artifact), trying the labeled source type first and falling back to the other scheme to
// support legacy or mislabeled deployments (mirroring resolveScheduledSourceRepo). ok is
// false when neither directory exists on disk, e.g. the data volume no longer has the
// checkout from a prior run.
func ResolveManagedSourceDir(dataMountPath, repositoryURL, labeledSourceType string) (path string, resolvedType config.SourceType, ok bool) {
	labeled := config.NormalizeSourceType(config.SourceType(labeledSourceType))
	if strings.TrimSpace(repositoryURL) == "" {
		return "", labeled, false
	}

	other := config.SourceTypeGit
	if labeled == config.SourceTypeGit {
		other = config.SourceTypeOCI
	}

	preferredName := ManagedSourceName(repositoryURL, string(labeled))

	preferredPath, err := filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPath, preferredName), dataMountPath)
	if err == nil && filesystem.IsDir(preferredPath) {
		return preferredPath, labeled, true
	}

	alternativeName := ManagedSourceName(repositoryURL, string(other))
	if alternativeName == preferredName {
		return preferredPath, labeled, false
	}

	alternativePath, err := filesystem.VerifyAndSanitizePath(filepath.Join(dataMountPath, alternativeName), dataMountPath)
	if err == nil && filesystem.IsDir(alternativePath) {
		return alternativePath, other, true
	}

	return preferredPath, labeled, false
}
