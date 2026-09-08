package docker

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/git"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/test"
)

func TestRecreateProjectStandard(t *testing.T) {
	ctx := context.Background()
	stack := test.ComposeUp(ctx, t, test.WithYAML(`services:
  web:
    image: nginx:latest
`))

	before, err := GetProjectContainers(ctx, stack.DockerCli, stack.Name)
	if err != nil {
		t.Fatalf("GetProjectContainers() before recreation error = %v", err)
	}

	if len(before) != 1 {
		t.Fatalf("containers before recreation = %d, want 1", len(before))
	}

	if err := RecreateProject(ctx, "", stack.DockerCli, stack.Name, "web", 30*time.Second, nil, ScheduledComposeOptions{}); err != nil {
		t.Fatalf("RecreateProject() error = %v", err)
	}

	after, err := GetProjectContainers(ctx, stack.DockerCli, stack.Name)
	if err != nil {
		t.Fatalf("GetProjectContainers() after recreation error = %v", err)
	}

	if len(after) != 1 {
		t.Fatalf("containers after recreation = %d, want 1", len(after))
	}

	if after[0].ID == before[0].ID {
		t.Fatal("RecreateProject() did not replace the service container")
	}

	err = RecreateProject(ctx, "", stack.DockerCli, test.ConvertTestName(t.Name())+"-missing", "", 30*time.Second, nil, ScheduledComposeOptions{})
	if err == nil || !strings.Contains(err.Error(), "project not found or has no containers") {
		t.Fatalf("RecreateProject() missing project error = %v", err)
	}
}

func TestRecreateProjectManaged(t *testing.T) {
	ctx := context.Background()
	dataMountPath := t.TempDir()
	repositoryURL := "https://example.com/owner/repo"
	repoPath := filepath.Join(dataMountPath, git.GetRepoName(repositoryURL))
	projectName := "managed-recreate"

	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}

	composePath := filepath.Join(repoPath, "compose.yml")
	if err := os.WriteFile(composePath, []byte(`services:
  web:
    image: nginx:latest
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repoPath, ".doco-cd.yml"), []byte(`name: managed-recreate
reference: refs/heads/main
working_dir: .
compose_files:
  - compose.yml
`), 0o600); err != nil {
		t.Fatal(err)
	}

	repo, err := gogit.PlainInit(repoPath, false)
	if err != nil {
		t.Fatal(err)
	}

	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatal(err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"compose.yml", ".doco-cd.yml"} {
		if _, err := worktree.Add(path); err != nil {
			t.Fatal(err)
		}
	}

	commit, err := worktree.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}

	stack := test.ComposeUp(ctx, t,
		test.WithFile(composePath),
		test.WithName(projectName),
		test.WithCustomLabel(map[string]string{
			DocoCDLabels.Deployment.Name:       projectName,
			DocoCDLabels.Deployment.TargetRef:  "refs/heads/main",
			DocoCDLabels.Deployment.CommitSHA:  commit.String(),
			DocoCDLabels.Deployment.ConfigHash: "deployed-config-hash",
			DocoCDLabels.Source.Type:           string(config.SourceTypeGit),
			DocoCDLabels.Source.Name:           "owner/repo",
			DocoCDLabels.Source.URL:            repositoryURL,
		}),
	)

	before, err := GetProjectContainers(ctx, stack.DockerCli, projectName)
	if err != nil {
		t.Fatalf("GetProjectContainers() before recreation error = %v", err)
	}

	if len(before) != 1 {
		t.Fatalf("containers before recreation = %d, want 1", len(before))
	}

	if err := RecreateProject(ctx, "", stack.DockerCli, projectName, "web", 30*time.Second, nil, ScheduledComposeOptions{
		ComposeLoad:         ComposeLoadOptions{DataMountPath: dataMountPath},
		DeployConfigBaseDir: "/",
	}); err != nil {
		t.Fatalf("RecreateProject() error = %v", err)
	}

	after, err := GetProjectContainers(ctx, stack.DockerCli, projectName)
	if err != nil {
		t.Fatalf("GetProjectContainers() after recreation error = %v", err)
	}

	if len(after) != 1 {
		t.Fatalf("containers after recreation = %d, want 1", len(after))
	}

	if after[0].ID == before[0].ID {
		t.Fatal("RecreateProject() did not replace the managed service container")
	}

	if got := after[0].Labels[DocoCDLabels.Deployment.ConfigHash]; got != "deployed-config-hash" {
		t.Fatalf("config hash label = %q, want deployed-config-hash", got)
	}
}

func TestRecreateProjectLabels(t *testing.T) {
	t.Parallel()

	fallback := map[string]string{api.WorkingDirLabel: "/srv/fallback"}
	managed := map[string]string{
		DocoCDLabels.Deployment.Name: "managed",
		DocoCDLabels.Source.URL:      "https://example.com/owner/repo",
	}

	testCases := []struct {
		name       string
		containers []api.ContainerSummary
		want       map[string]string
	}{
		{
			name: "prefers managed metadata",
			containers: []api.ContainerSummary{
				{Labels: fallback},
				{Labels: managed},
			},
			want: managed,
		},
		{
			name:       "falls back to Compose metadata",
			containers: []api.ContainerSummary{{Labels: fallback}},
			want:       fallback,
		},
		{
			name:       "returns nil without usable metadata",
			containers: []api.ContainerSummary{{Labels: map[string]string{}}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recreateProjectLabels(tc.containers); !maps.Equal(got, tc.want) {
				t.Fatalf("recreateProjectLabels() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSelectRecreateServices(t *testing.T) {
	project := &types.Project{
		Name: "example",
		Services: types.Services{
			"web": {
				Name: "web",
				DependsOn: types.DependsOnConfig{
					"db": {},
				},
			},
			"db":       {Name: "db"},
			"inactive": {Name: "inactive"},
		},
	}

	t.Run("specific service includes dependencies", func(t *testing.T) {
		selected, targets, err := selectRecreateServices(project, "web", nil)
		if err != nil {
			t.Fatal(err)
		}

		if len(selected.Services) != 2 {
			t.Fatalf("selected services = %v, want web and db", selected.ServiceNames())
		}

		if len(targets) != 1 || targets[0] != "web" {
			t.Fatalf("recreate targets = %v, want [web]", targets)
		}
	})

	t.Run("whole project excludes inactive profiles", func(t *testing.T) {
		selected, targets, err := selectRecreateServices(project, "", []string{"web"})
		if err != nil {
			t.Fatal(err)
		}

		if len(selected.Services) != 2 {
			t.Fatalf("selected services = %v, want web and db", selected.ServiceNames())
		}

		if targets != nil {
			t.Fatalf("recreate targets = %v, want nil", targets)
		}
	})

	t.Run("missing service", func(t *testing.T) {
		_, _, err := selectRecreateServices(project, "missing", nil)
		if !errors.Is(err, ErrComposeServiceNotFound) {
			t.Fatalf("error = %v, want ErrComposeServiceNotFound", err)
		}
	})

	t.Run("no active services keeps full project", func(t *testing.T) {
		selected, targets, err := selectRecreateServices(project, "", nil)
		if err != nil {
			t.Fatal(err)
		}

		if selected != project || targets != nil {
			t.Fatalf("selectRecreateServices() = (%v, %v), want original project and nil targets", selected, targets)
		}
	})

	t.Run("unknown active service", func(t *testing.T) {
		_, _, err := selectRecreateServices(project, "", []string{"missing"})
		if err == nil {
			t.Fatal("selectRecreateServices() succeeded for unknown active service")
		}
	})
}

func TestAddComposeServiceTrackingLabels(t *testing.T) {
	project := &types.Project{
		Name:         "example",
		WorkingDir:   "/srv/example",
		ComposeFiles: []string{"/srv/example/compose.yml"},
		Services: types.Services{
			"web": {
				Name: "web",
				CustomLabels: map[string]string{
					"example.com/custom":     "preserved",
					api.EnvironmentFileLabel: "/srv/example/.env",
				},
			},
		},
	}

	addComposeServiceTrackingLabels(project)

	labels := project.Services["web"].CustomLabels
	for key, want := range map[string]string{
		"example.com/custom":     "preserved",
		api.EnvironmentFileLabel: "/srv/example/.env",
		api.ProjectLabel:         "example",
		api.ServiceLabel:         "web",
		api.WorkingDirLabel:      "/srv/example",
		api.ConfigFilesLabel:     "/srv/example/compose.yml",
		api.VersionLabel:         api.ComposeVersion,
		api.OneoffLabel:          "False",
	} {
		if got := labels[key]; got != want {
			t.Errorf("label %q = %q, want %q", key, got, want)
		}
	}
}

func TestValidateManagedRecreateRevision(t *testing.T) {
	t.Run("matching Git HEAD", func(t *testing.T) {
		dataMountPath := t.TempDir()
		ref := composeScheduledServiceRef{
			Project:       "example",
			RepositoryURL: "https://example.com/owner/repo",
			SourceType:    string(config.SourceTypeGit),
		}

		repoPath := filepath.Join(dataMountPath, git.GetRepoName(ref.RepositoryURL))
		if err := os.MkdirAll(repoPath, 0o755); err != nil {
			t.Fatal(err)
		}

		repo, err := gogit.PlainInit(repoPath, false)
		if err != nil {
			t.Fatal(err)
		}

		if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(repoPath, "compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		worktree, err := repo.Worktree()
		if err != nil {
			t.Fatal(err)
		}

		if _, err := worktree.Add("compose.yml"); err != nil {
			t.Fatal(err)
		}

		commit, err := worktree.Commit("initial", &gogit.CommitOptions{
			Author: &object.Signature{Name: "test", Email: "test@example.com"},
		})
		if err != nil {
			t.Fatal(err)
		}

		err = validateManagedRecreateRevision(ref, map[string]string{
			DocoCDLabels.Deployment.CommitSHA: commit.String(),
		}, ScheduledComposeOptions{ComposeLoad: ComposeLoadOptions{DataMountPath: dataMountPath}}, repoPath, config.SourceTypeGit)
		if err != nil {
			t.Fatalf("validateManagedRecreateRevision() error = %v", err)
		}
	})

	t.Run("mismatched Git HEAD", func(t *testing.T) {
		dataMountPath := t.TempDir()
		ref := composeScheduledServiceRef{
			Project:       "example",
			RepositoryURL: "https://example.com/owner/repo",
			SourceType:    string(config.SourceTypeGit),
		}

		repoPath := filepath.Join(dataMountPath, git.GetRepoName(ref.RepositoryURL))

		err := validateManagedRecreateRevision(ref, map[string]string{
			DocoCDLabels.Deployment.CommitSHA: "deadbeef",
		}, ScheduledComposeOptions{ComposeLoad: ComposeLoadOptions{DataMountPath: dataMountPath}}, repoPath, config.SourceTypeGit)
		if !errors.Is(err, ErrComposeSourceRevisionConflict) {
			t.Fatalf("validateManagedRecreateRevision() error = %v, want ErrComposeSourceRevisionConflict", err)
		}
	})

	t.Run("missing deployed revision", func(t *testing.T) {
		err := validateManagedRecreateRevision(composeScheduledServiceRef{Project: "example"}, nil, ScheduledComposeOptions{}, "", config.SourceTypeGit)
		if !errors.Is(err, ErrComposeSourceRevisionConflict) {
			t.Fatalf("validateManagedRecreateRevision() error = %v, want ErrComposeSourceRevisionConflict", err)
		}
	})

	t.Run("matching OCI marker", func(t *testing.T) {
		dataMountPath := t.TempDir()
		ref := composeScheduledServiceRef{
			Project:       "example",
			RepositoryURL: "registry.example.com/owner/artifact:latest",
			SourceType:    string(config.SourceTypeOCI),
		}

		sourcePath, sourceType, err := resolveScheduledSourceRepo(ref, dataMountPath)
		if err != nil {
			t.Fatal(err)
		}

		if err := sourcecache.WriteRevision(dataMountPath, sourcePath, sourceType, "sha256:deployed"); err != nil {
			t.Fatal(err)
		}

		err = validateManagedRecreateRevision(ref, map[string]string{
			DocoCDLabels.Deployment.CommitSHA: "sha256:deployed",
		}, ScheduledComposeOptions{ComposeLoad: ComposeLoadOptions{DataMountPath: dataMountPath}}, sourcePath, sourceType)
		if err != nil {
			t.Fatalf("validateManagedRecreateRevision() error = %v", err)
		}
	})

	for _, tc := range []struct {
		name           string
		cachedRevision string
	}{
		{name: "mismatched OCI marker", cachedRevision: "sha256:newer"},
		{name: "missing OCI marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataMountPath := t.TempDir()
			ref := composeScheduledServiceRef{
				Project:       "example",
				RepositoryURL: "registry.example.com/owner/artifact:latest",
				SourceType:    string(config.SourceTypeOCI),
			}

			sourcePath, sourceType, err := resolveScheduledSourceRepo(ref, dataMountPath)
			if err != nil {
				t.Fatal(err)
			}

			if tc.cachedRevision != "" {
				if err := sourcecache.WriteRevision(dataMountPath, sourcePath, sourceType, tc.cachedRevision); err != nil {
					t.Fatal(err)
				}
			}

			err = validateManagedRecreateRevision(ref, map[string]string{
				DocoCDLabels.Deployment.CommitSHA: "sha256:deployed",
			}, ScheduledComposeOptions{ComposeLoad: ComposeLoadOptions{DataMountPath: dataMountPath}}, sourcePath, sourceType)
			if !errors.Is(err, ErrComposeSourceRevisionConflict) {
				t.Fatalf("validateManagedRecreateRevision() error = %v, want ErrComposeSourceRevisionConflict", err)
			}
		})
	}
}

func TestRestoreDeploymentConfigHash(t *testing.T) {
	t.Run("preserves existing hash", func(t *testing.T) {
		deployConfig := &deploy.Config{}
		deployConfig.Internal.Hash = "reloaded-hash"

		restoreDeploymentConfigHash(deployConfig, map[string]string{
			DocoCDLabels.Deployment.ConfigHash: "deployed-hash",
		})

		if deployConfig.Internal.Hash != "deployed-hash" {
			t.Fatalf("config hash = %q, want deployed hash", deployConfig.Internal.Hash)
		}
	})

	t.Run("keeps reloaded hash when label is absent", func(t *testing.T) {
		deployConfig := &deploy.Config{}
		deployConfig.Internal.Hash = "reloaded-hash"

		restoreDeploymentConfigHash(deployConfig, nil)

		if deployConfig.Internal.Hash != "reloaded-hash" {
			t.Fatalf("config hash = %q, want reloaded hash", deployConfig.Internal.Hash)
		}
	})
}
