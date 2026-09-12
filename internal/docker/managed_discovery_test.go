package docker

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
)

func TestDiscoverManagedDeployments(t *testing.T) {
	t.Parallel()

	containers := []container.Summary{
		{
			ID: "container-1",
			Labels: map[string]string{
				DocoCDLabels.Source.Name:             "owner/repo-a",
				DocoCDLabels.Source.URL:              "https://github.com/owner/repo-a.git",
				DocoCDLabels.Source.Type:             "git",
				DocoCDLabels.Deployment.Name:         "repo-a-base",
				DocoCDLabels.Deployment.ConfigTarget: "",
				DocoCDLabels.Deployment.TargetRef:    "main",
			},
		},
		{
			// Same repository, different config target -> should add a second target entry.
			ID: "container-2",
			Labels: map[string]string{
				DocoCDLabels.Source.Name:             "owner/repo-a",
				DocoCDLabels.Source.URL:              "https://github.com/owner/repo-a.git",
				DocoCDLabels.Source.Type:             "git",
				DocoCDLabels.Deployment.Name:         "repo-a-nas",
				DocoCDLabels.Deployment.ConfigTarget: "nas",
				DocoCDLabels.Deployment.TargetRef:    "main",
			},
		},
		{
			// Duplicate of container-1's target -> should not add a duplicate entry.
			ID: "container-3",
			Labels: map[string]string{
				DocoCDLabels.Source.Name:             "owner/repo-a",
				DocoCDLabels.Source.URL:              "https://github.com/owner/repo-a.git",
				DocoCDLabels.Source.Type:             "git",
				DocoCDLabels.Deployment.Name:         "repo-a-base",
				DocoCDLabels.Deployment.ConfigTarget: "",
				DocoCDLabels.Deployment.TargetRef:    "main",
			},
		},
		{
			// Different repository entirely.
			ID: "container-4",
			Labels: map[string]string{
				DocoCDLabels.Source.Name:     "owner/repo-b",
				DocoCDLabels.Source.URL:      "https://github.com/owner/repo-b.git",
				DocoCDLabels.Source.Type:     "oci",
				DocoCDLabels.Deployment.Name: "repo-b",
			},
		},
		{
			// Missing the repository label entirely -> must be ignored.
			ID:     "container-5",
			Labels: map[string]string{},
		},
	}

	services := []swarm.Service{
		{
			ID: "service-1",
			Spec: swarm.ServiceSpec{
				Annotations: swarm.Annotations{
					Labels: map[string]string{
						DocoCDLabels.Source.Name:             "owner/repo-c",
						DocoCDLabels.Source.URL:              "https://github.com/owner/repo-c.git",
						DocoCDLabels.Source.Type:             "git",
						DocoCDLabels.Deployment.Name:         "repo-c-prod",
						DocoCDLabels.Deployment.ConfigTarget: "prod",
						DocoCDLabels.Deployment.TargetRef:    "v1.0.0",
					},
				},
			},
		},
	}

	t.Run("swarm mode disabled ignores services", func(t *testing.T) {
		t.Parallel()

		apiClient := &runtimeQueryTestClient{containers: containers, services: services}

		refs, err := DiscoverManagedDeployments(t.Context(), apiClient, "", false)
		if err != nil {
			t.Fatal(err)
		}

		if len(refs) != 2 {
			t.Fatalf("got %d refs, want 2 (repo-a, repo-b): %#v", len(refs), refs)
		}

		if !apiClient.containerOptions.All {
			t.Fatal("expected ContainerList to be called with All=true")
		}

		repoA := findManagedDeploymentRef(t, refs, "owner/repo-a")

		if len(repoA.Targets) != 2 {
			t.Fatalf("repo-a targets = %#v, want 2 distinct targets (deduped)", repoA.Targets)
		}

		repoB := findManagedDeploymentRef(t, refs, "owner/repo-b")
		if repoB.SourceType != "oci" {
			t.Fatalf("repo-b source type = %q, want %q", repoB.SourceType, "oci")
		}
	})

	t.Run("swarm mode enabled includes services", func(t *testing.T) {
		t.Parallel()

		apiClient := &runtimeQueryTestClient{containers: containers, services: services}

		refs, err := DiscoverManagedDeployments(t.Context(), apiClient, "", true)
		if err != nil {
			t.Fatal(err)
		}

		if len(refs) != 3 {
			t.Fatalf("got %d refs, want 3 (repo-a, repo-b, repo-c): %#v", len(refs), refs)
		}

		repoC := findManagedDeploymentRef(t, refs, "owner/repo-c")

		if len(repoC.Targets) != 1 || repoC.Targets[0].ConfigTarget != "prod" || repoC.Targets[0].Reference != "v1.0.0" {
			t.Fatalf("repo-c targets = %#v, want single {prod, v1.0.0} target", repoC.Targets)
		}
	})
}

func findManagedDeploymentRef(t *testing.T, refs []ManagedDeploymentRef, name string) ManagedDeploymentRef {
	t.Helper()

	for _, ref := range refs {
		if ref.RepositoryName == name {
			return ref
		}
	}

	t.Fatalf("no ref found for repository %q in %#v", name, refs)

	return ManagedDeploymentRef{}
}

func TestDiscoverManagedDeployments_UsesManagerLabelFilter(t *testing.T) {
	t.Parallel()

	apiClient := &runtimeQueryTestClient{}

	if _, err := DiscoverManagedDeployments(t.Context(), apiClient, "", false); err != nil {
		t.Fatal(err)
	}

	wantFilters := make(client.Filters)
	wantFilters.Add("label", DocoCDLabels.Metadata.Manager+"=doco-cd")

	if !reflect.DeepEqual(apiClient.containerOptions.Filters, wantFilters) {
		t.Fatalf("container filters = %#v, want %#v", apiClient.containerOptions.Filters, wantFilters)
	}
}

func TestDiscoverManagedDeployments_DistinguishesRepositoryHosts(t *testing.T) {
	t.Parallel()

	apiClient := &runtimeQueryTestClient{
		containers: []container.Summary{
			{
				ID: "github-container",
				Labels: map[string]string{
					DocoCDLabels.Source.Name:     "owner/repo",
					DocoCDLabels.Source.URL:      "https://github.com/owner/repo.git",
					DocoCDLabels.Source.Type:     "git",
					DocoCDLabels.Deployment.Name: "github-stack",
				},
			},
			{
				ID: "gitlab-container",
				Labels: map[string]string{
					DocoCDLabels.Source.Name:     "owner/repo",
					DocoCDLabels.Source.URL:      "https://gitlab.com/owner/repo.git",
					DocoCDLabels.Source.Type:     "git",
					DocoCDLabels.Deployment.Name: "gitlab-stack",
				},
			},
		},
	}

	refs, err := DiscoverManagedDeployments(t.Context(), apiClient, "", false)
	if err != nil {
		t.Fatal(err)
	}

	if len(refs) != 2 {
		t.Fatalf("got %d refs, want distinct GitHub and GitLab repositories: %#v", len(refs), refs)
	}
}

func TestResolveManagedSourceDir(t *testing.T) {
	t.Parallel()

	dataMountPath := t.TempDir()

	gitRepoName := "github.com/owner/git-repo"
	if err := os.MkdirAll(filepath.Join(dataMountPath, gitRepoName), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("finds labeled git source directory", func(t *testing.T) {
		t.Parallel()

		source, ok := ResolveManagedSourceDir(dataMountPath, "https://github.com/owner/git-repo.git", "git")
		if !ok {
			t.Fatal("expected to find existing git checkout")
		}

		if source.Type != config.SourceTypeGit {
			t.Fatalf("source type = %q, want %q", source.Type, config.SourceTypeGit)
		}

		if source.Name != gitRepoName {
			t.Fatalf("source name = %q, want %q", source.Name, gitRepoName)
		}

		if filepath.Clean(source.Path) != filepath.Join(dataMountPath, gitRepoName) {
			t.Fatalf("path = %q, want %q", source.Path, filepath.Join(dataMountPath, gitRepoName))
		}
	})

	t.Run("falls back to the other source type when the labeled one is missing", func(t *testing.T) {
		t.Parallel()

		// Labeled as OCI, but only the git-named directory exists on disk.
		source, ok := ResolveManagedSourceDir(dataMountPath, "https://github.com/owner/git-repo.git", "oci")
		if !ok {
			t.Fatal("expected fallback to the git checkout to succeed")
		}

		if source.Type != config.SourceTypeGit {
			t.Fatalf("source type = %q, want %q", source.Type, config.SourceTypeGit)
		}
	})

	t.Run("reports not found when neither directory exists", func(t *testing.T) {
		t.Parallel()

		_, ok := ResolveManagedSourceDir(dataMountPath, "https://github.com/owner/missing-repo.git", "git")
		if ok {
			t.Fatal("expected not to find a checkout for a repository never deployed")
		}
	})
}
