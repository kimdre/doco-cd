package docker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/git"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

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
