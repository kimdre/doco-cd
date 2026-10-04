package stages

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

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

	labeled := func(names ...string) map[docker.Service]docker.Labels {
		services := make(map[docker.Service]docker.Labels, len(names))
		for i, name := range names {
			services[docker.Service(fmt.Sprintf("service-%d", i))] = docker.Labels{docker.DocoCDLabels.Source.Name: name}
		}

		return services
	}

	tests := []struct {
		name       string
		services   map[docker.Service]docker.Labels
		repository *RepositoryData
		want       bool
	}{
		{name: "no services", services: labeled(), repository: gitRepository, want: true},
		{name: "git webhook label", services: labeled("owner/config"), repository: gitRepository, want: true},
		{name: "git repository name label", services: labeled("github.com/owner/config"), repository: gitRepository, want: true},
		{name: "other git repository", services: labeled("owner/other"), repository: gitRepository, want: false},
		{name: "missing label", services: map[docker.Service]docker.Labels{"app": {}}, repository: gitRepository, want: false},
		{name: "one service of another repository", services: labeled("owner/config", "owner/other"), repository: gitRepository, want: false},
		{name: "repository_url deployed by webhook", services: labeled("owner/config"), repository: remoteRepository, want: true},
		{name: "repository_url deployed by poll", services: labeled("owner/app"), repository: remoteRepository, want: true},
		{name: "oci deployed by poll with another tag", services: labeled("ghcr.io/org/app"), repository: ociRepository, want: true},
		{name: "oci deployed by webhook", services: labeled("org/app"), repository: ociRepository, want: true},
		{name: "oci digest reference", services: labeled("ghcr.io/org/app"), repository: ociDigestRepository, want: true},
		{name: "other oci repository", services: labeled("ghcr.io/org/other"), repository: ociRepository, want: false},
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
