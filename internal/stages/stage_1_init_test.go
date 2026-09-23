package stages

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
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
