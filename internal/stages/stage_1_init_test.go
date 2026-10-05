package stages

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
)

func TestMergeDeploymentEnvironment(t *testing.T) {
	t.Parallel()

	config := deploy.New("app", "main")
	config.Environment = map[string]string{
		"SHARED": "configured",
		"EXTRA":  "value",
	}
	config.Internal.Environment = map[string]string{
		"SHARED":      "dotenv",
		"DOTENV_ONLY": "preserved",
	}

	mergeDeploymentEnvironment(config)

	want := map[string]string{
		"SHARED":      "configured",
		"EXTRA":       "value",
		"DOTENV_ONLY": "preserved",
	}
	if !reflect.DeepEqual(config.Internal.Environment, want) {
		t.Fatalf("Internal.Environment = %v, want %v", config.Internal.Environment, want)
	}
}

func TestMergeDeploymentEnvironmentInitializesInternalEnvironment(t *testing.T) {
	t.Parallel()

	config := deploy.New("app", "main")
	config.Environment = map[string]string{"APP_ENV": "production"}

	mergeDeploymentEnvironment(config)

	if got := config.Internal.Environment["APP_ENV"]; got != "production" {
		t.Fatalf("Internal.Environment[APP_ENV] = %q, want %q", got, "production")
	}
}

func TestSourceURLForLabels(t *testing.T) {
	t.Parallel()

	t.Run("keeps config source when deployment uses another repository", func(t *testing.T) {
		t.Parallel()

		repository := &RepositoryData{
			SourceUrl:       "ssh://git@deploy.example.com/owner/app.git",
			ConfigSourceUrl: "ssh://git@config.example.com/owner/config.git",
		}

		if got := sourceURLForLabels(repository); got != repository.ConfigSourceUrl {
			t.Fatalf("sourceURLForLabels() = %q, want config source %q", got, repository.ConfigSourceUrl)
		}
	})

	t.Run("falls back for directly constructed repository data", func(t *testing.T) {
		t.Parallel()

		repository := &RepositoryData{SourceUrl: "ssh://git@example.com/owner/repo.git"}

		if got := sourceURLForLabels(repository); got != repository.SourceUrl {
			t.Fatalf("sourceURLForLabels() = %q, want source %q", got, repository.SourceUrl)
		}
	})
}

func TestUseDeploymentGitRepositorySwitchesOCIConfigSourceToGit(t *testing.T) {
	t.Parallel()

	repository := &RepositoryData{
		Source:          config.SourceTypeOCI,
		SourceUrl:       "ghcr.io/owner/config:latest",
		ConfigSourceUrl: "ghcr.io/owner/config:latest",
	}

	useDeploymentGitRepository(repository, "https://github.com/owner/app.git")

	if repository.Source != config.SourceTypeGit {
		t.Fatalf("Source = %q, want %q", repository.Source, config.SourceTypeGit)
	}

	if repository.SourceUrl != "https://github.com/owner/app.git" {
		t.Fatalf("SourceUrl = %q, want deployment Git URL", repository.SourceUrl)
	}

	if repository.Name != "github.com/owner/app" {
		t.Fatalf("Name = %q, want deployment Git store name", repository.Name)
	}

	if repository.ConfigSourceUrl != "ghcr.io/owner/config:latest" {
		t.Fatalf("ConfigSourceUrl = %q, want original OCI config source", repository.ConfigSourceUrl)
	}
}

func TestLoadConfigSourceFilesReusesPreviouslyLoadedValues(t *testing.T) {
	t.Parallel()

	deployConfig := deploy.New("app", "main")
	deployConfig.Internal.ConfigSourceFilesLoaded = true
	deployConfig.Internal.Environment = map[string]string{"FROM_CONFIG_SOURCE": "preserved"}

	if err := loadConfigSourceFiles(deployConfig, filepath.Join(t.TempDir(), "removed-artifact")); err != nil {
		t.Fatalf("loadConfigSourceFiles() error = %v", err)
	}

	if got := deployConfig.Internal.Environment["FROM_CONFIG_SOURCE"]; got != "preserved" {
		t.Fatalf("loaded environment value = %q, want preserved", got)
	}
}

func TestServicesBelongToRepository(t *testing.T) {
	t.Parallel()

	gitRepository := &RepositoryData{
		Source:          config.SourceTypeGit,
		Name:            "github.com/owner/config",
		SourceUrl:       "https://github.com/owner/config.git",
		ConfigSourceUrl: "https://github.com/owner/config.git",
	}
	// A deploy config with a repository_url is deployed from another repository than it was read from.
	remoteRepository := &RepositoryData{
		Source:          config.SourceTypeGit,
		Name:            "github.com/owner/app",
		SourceUrl:       "https://github.com/owner/app.git",
		ConfigSourceUrl: "https://github.com/owner/config.git",
	}
	ociRepository := &RepositoryData{
		Source:          config.SourceTypeOCI,
		Name:            "ghcr.io/org/app",
		SourceUrl:       "ghcr.io/org/app:v2",
		ConfigSourceUrl: "ghcr.io/org/app:v2",
	}
	ociDigestRepository := &RepositoryData{
		Source:          config.SourceTypeOCI,
		Name:            "ghcr.io/org/app",
		SourceUrl:       "ghcr.io/org/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ConfigSourceUrl: "ghcr.io/org/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	ociRemoteRepository := &RepositoryData{
		Source:          config.SourceTypeGit,
		Name:            "github.com/owner/app",
		SourceUrl:       "https://github.com/owner/app.git",
		ConfigSourceUrl: "ghcr.io/org/config:latest",
	}

	labeled := func(names ...string) map[docker.Service]docker.Labels {
		services := make(map[docker.Service]docker.Labels, len(names))
		for i, name := range names {
			services[docker.Service(fmt.Sprintf("service-%d", i))] = docker.Labels{docker.DocoCDLabels.Source.Name: name}
		}

		return services
	}

	withSource := func(sourceURL, sourceType string, names ...string) map[docker.Service]docker.Labels {
		services := labeled(names...)
		for _, labels := range services {
			labels[docker.DocoCDLabels.Source.URL] = sourceURL
			labels[docker.DocoCDLabels.Source.Type] = sourceType
		}

		return services
	}

	gitLabeled := func(names ...string) map[docker.Service]docker.Labels {
		return withSource("https://github.com/owner/config.git", "git", names...)
	}
	ociLabeled := func(names ...string) map[docker.Service]docker.Labels {
		return withSource("ghcr.io/org/app:old", "oci", names...)
	}

	tests := []struct {
		name       string
		services   map[docker.Service]docker.Labels
		repository *RepositoryData
		want       bool
	}{
		{name: "no services", services: labeled(), repository: gitRepository, want: true},
		{name: "git webhook label", services: gitLabeled("owner/config"), repository: gitRepository, want: true},
		{name: "git repository name label", services: gitLabeled("github.com/owner/config"), repository: gitRepository, want: true},
		{name: "git SSH URL spelling", services: withSource("git@github.com:owner/config.git", "git", "owner/config"), repository: gitRepository, want: true},
		{name: "other git repository", services: gitLabeled("owner/other"), repository: gitRepository, want: false},
		{name: "missing label", services: map[docker.Service]docker.Labels{"app": {}}, repository: gitRepository, want: false},
		{name: "one service of another repository", services: gitLabeled("owner/config", "owner/other"), repository: gitRepository, want: false},
		{name: "repository_url deployed by webhook", services: gitLabeled("owner/config"), repository: remoteRepository, want: true},
		{name: "repository_url deployed by poll", services: gitLabeled("owner/app"), repository: remoteRepository, want: true},
		{name: "legacy repository_url deployment URL", services: withSource("https://github.com/owner/app.git", "git", "owner/app"), repository: remoteRepository, want: true},
		{name: "oci deployed by poll with another tag", services: ociLabeled("ghcr.io/org/app"), repository: ociRepository, want: true},
		{name: "oci deployed by webhook", services: ociLabeled("org/app"), repository: ociRepository, want: true},
		{name: "oci digest reference", services: ociLabeled("ghcr.io/org/app"), repository: ociDigestRepository, want: true},
		{name: "oci digest URL against tagged request", services: withSource(ociDigestRepository.SourceUrl, "oci", "org/app"), repository: ociRepository, want: true},
		{name: "foreign registry with same webhook name", services: withSource("registry.example.com/org/app:latest", "oci", "org/app"), repository: ociRepository, want: false},
		{name: "foreign Git source with same webhook name", services: withSource("https://github.com/org/app.git", "git", "org/app"), repository: ociRepository, want: false},
		{name: "Git source on same registry host is not OCI", services: withSource("https://ghcr.io/org/app.git", "oci", "org/app"), repository: ociRepository, want: false},
		{name: "foreign Git host with same webhook name", services: withSource("https://foreign.example.com/owner/config.git", "git", "owner/config"), repository: gitRepository, want: false},
		{name: "wrong source type", services: withSource("ghcr.io/org/app:latest", "git", "org/app"), repository: ociRepository, want: false},
		{name: "legacy OCI URL without type", services: withSource("ghcr.io/org/app:latest", "", "org/app"), repository: ociRepository, want: true},
		{name: "legacy Git URL without type", services: withSource("https://github.com/owner/config", "", "owner/config"), repository: gitRepository, want: true},
		{name: "legacy qualified Git name with type", services: withSource("", "git", "github.com/owner/config"), repository: gitRepository, want: true},
		{name: "legacy qualified OCI name with type", services: withSource("", "oci", "ghcr.io/org/app"), repository: ociRepository, want: true},
		{name: "hostless legacy Git name is ambiguous", services: labeled("owner/config"), repository: gitRepository, want: false},
		{name: "hostless legacy OCI name is ambiguous", services: withSource("", "oci", "org/app"), repository: ociRepository, want: false},
		{name: "qualified name without URL or type is ambiguous", services: labeled("ghcr.io/org/app"), repository: ociRepository, want: false},
		{name: "invalid URL cannot fall back to matching name", services: withSource("invalid://org/app", "oci", "ghcr.io/org/app"), repository: ociRepository, want: false},
		{name: "unknown type cannot fall back to matching URL", services: withSource("ghcr.io/org/app:latest", "unknown", "org/app"), repository: ociRepository, want: false},
		{name: "OCI config repository_url webhook", services: withSource("ghcr.io/org/config:older", "oci", "org/config"), repository: ociRemoteRepository, want: true},
		{name: "OCI config repository_url poll", services: withSource("ghcr.io/org/config:older", "git", "owner/app"), repository: ociRemoteRepository, want: true},
		{name: "OCI config repository_url poll with qualified deployment name", services: withSource("ghcr.io/org/config:older", "git", "github.com/owner/app"), repository: ociRemoteRepository, want: true},
		{name: "OCI config Git type with config name is not a deployment poll", services: withSource("ghcr.io/org/config:older", "git", "org/config"), repository: ociRemoteRepository, want: false},
		{name: "OCI config with legacy Git deployment URL", services: withSource("https://github.com/owner/app.git", "git", "owner/app"), repository: ociRemoteRepository, want: true},
		{name: "OCI config type cannot authorize a Git deployment URL through a short name", services: withSource("https://github.com/owner/app.git", "oci", "owner/app"), repository: ociRemoteRepository, want: false},
		{name: "OCI config type cannot authorize a Git deployment URL through its config name", services: withSource("https://github.com/owner/app.git", "oci", "org/config"), repository: ociRemoteRepository, want: false},
		{name: "OCI repository_url foreign config registry", services: withSource("registry.example.com/org/config:latest", "oci", "org/config"), repository: ociRemoteRepository, want: false},
		{name: "OCI repository_url poll type cannot authorize a foreign registry", services: withSource("registry.example.com/org/config:latest", "git", "owner/app"), repository: ociRemoteRepository, want: false},
		{name: "other oci repository", services: labeled("ghcr.io/org/other"), repository: ociRepository, want: false},
		{name: "missing request source metadata", services: ociLabeled("org/app"), repository: &RepositoryData{Source: config.SourceTypeOCI, Name: "ghcr.io/org/app"}, want: false},
		{name: "nil request repository", services: ociLabeled("org/app"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := servicesBelongToRepository(tt.services, tt.repository); got != tt.want {
				t.Fatalf("servicesBelongToRepository() = %v, want %v", got, tt.want)
			}
		})
	}
}

// destroyOwnershipTestClient serves the containers of a Compose stack. Any other call panics via the nil embedded
// interface.
type destroyOwnershipTestClient struct {
	client.APIClient

	containers []container.Summary
	err        error
	options    client.ContainerListOptions
}

func (c *destroyOwnershipTestClient) ContainerList(_ context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	c.options = options
	return client.ContainerListResult{Items: c.containers}, c.err
}

type destroyOwnershipTestCli struct {
	command.Cli

	apiClient client.APIClient
}

func (c destroyOwnershipTestCli) Client() client.APIClient { return c.apiClient }

func TestCheckDestroyOwnership(t *testing.T) {
	t.Parallel()

	repository := &RepositoryData{
		Source:          config.SourceTypeGit,
		Name:            "github.com/owner/config",
		SourceUrl:       "https://github.com/owner/config.git",
		ConfigSourceUrl: "https://github.com/owner/config.git",
	}

	stackContainer := func(sourceName string) container.Summary {
		return container.Summary{
			Names: []string{"/app-web-1"},
			Labels: map[string]string{
				api.ProjectLabel:                "app",
				docker.DocoCDLabels.Source.Name: sourceName,
				docker.DocoCDLabels.Source.URL:  repository.SourceUrl,
				docker.DocoCDLabels.Source.Type: "git",
			},
		}
	}

	ociRepository := &RepositoryData{
		Source:          config.SourceTypeOCI,
		Name:            "ghcr.io/org/app",
		SourceUrl:       "ghcr.io/org/app:latest",
		ConfigSourceUrl: "ghcr.io/org/app:latest",
	}
	ociContainer := func(sourceURL, sourceType string) container.Summary {
		c := stackContainer("org/app")
		c.Labels[docker.DocoCDLabels.Source.URL] = sourceURL
		c.Labels[docker.DocoCDLabels.Source.Type] = sourceType

		return c
	}

	listErr := errors.New("daemon unavailable")

	tests := []struct {
		name       string
		destroy    bool
		containers []container.Summary
		repository *RepositoryData
		listErr    error
		wantErr    error
		wantList   bool
	}{
		// Docker is not queried at all if the deploy config does not destroy the stack.
		{name: "destroy disabled", destroy: false, containers: []container.Summary{stackContainer("owner/other")}},
		{name: "stack not deployed", destroy: true, wantList: true},
		{name: "stack deployed from repository", destroy: true, containers: []container.Summary{stackContainer("owner/config")}, wantList: true},
		{name: "stack deployed from other repository", destroy: true, containers: []container.Summary{stackContainer("owner/other")}, wantErr: ErrDeploymentConflict, wantList: true},
		{name: "OCI same repository different tag", destroy: true, repository: ociRepository, containers: []container.Summary{ociContainer("ghcr.io/org/app:production", "oci")}, wantList: true},
		{name: "OCI foreign registry same name", destroy: true, repository: ociRepository, containers: []container.Summary{ociContainer("registry.example.com/org/app:latest", "oci")}, wantErr: ErrDeploymentConflict, wantList: true},
		{name: "OCI foreign Git source same name", destroy: true, repository: ociRepository, containers: []container.Summary{ociContainer("https://github.com/org/app.git", "git")}, wantErr: ErrDeploymentConflict, wantList: true},
		{name: "OCI mixed ownership services", destroy: true, repository: ociRepository, containers: []container.Summary{ociContainer("ghcr.io/org/app:old", "oci"), ociContainer("registry.example.com/org/app:latest", "oci")}, wantErr: ErrDeploymentConflict, wantList: true},
		{name: "OCI missing URL and hostless name", destroy: true, repository: ociRepository, containers: []container.Summary{ociContainer("", "oci")}, wantErr: ErrDeploymentConflict, wantList: true},
		{name: "list error", destroy: true, listErr: listErr, wantErr: listErr, wantList: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			apiClient := &destroyOwnershipTestClient{containers: tt.containers, err: tt.listErr}

			deployConfig := deploy.New("app", "main")
			deployConfig.Destroy.Enabled = tt.destroy

			requestRepository := tt.repository
			if requestRepository == nil {
				requestRepository = repository
			}

			s := &StageManager{
				DeployConfig: deployConfig,
				Docker:       &Docker{Cmd: destroyOwnershipTestCli{apiClient: apiClient}},
				Repository:   requestRepository,
			}

			err := s.checkDestroyOwnership(t.Context())
			if tt.wantErr == nil && err != nil {
				t.Fatalf("checkDestroyOwnership() error = %v, want nil", err)
			}

			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("checkDestroyOwnership() error = %v, want %v", err, tt.wantErr)
			}

			if listed := apiClient.options.Filters != nil; listed != tt.wantList {
				t.Fatalf("listed containers = %v, want %v", listed, tt.wantList)
			}
		})
	}
}
