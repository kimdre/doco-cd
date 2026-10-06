package docker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/source/store"
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

	// Content lives under the store's per-revision artifact directory, not
	// flat at repoPath (repoPath is a GitStore base directory: mirror/ and artifacts/<revision>).
	// commitSHA stands in for the revision GitStore would have resolved and published;
	// no real Git history is needed since validateManagedRecreateRevision only checks that the artifact directory
	// exists, and this deploy config has auto-discovery disabled.
	commitSHA := "b1946ac92492d2347c6235b4d2611184"
	artifactPath := filepath.Join(repoPath, "artifacts", commitSHA)

	if err := os.MkdirAll(artifactPath, 0o755); err != nil {
		t.Fatal(err)
	}

	composePath := filepath.Join(artifactPath, "compose.yml")
	if err := os.WriteFile(composePath, []byte(`services:
  web:
    image: nginx:latest
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(artifactPath, ".doco-cd.yml"), []byte(`name: managed-recreate
reference: refs/heads/main
working_dir: .
compose_files:
  - compose.yml
`), 0o600); err != nil {
		t.Fatal(err)
	}

	stack := test.ComposeUp(ctx, t,
		test.WithFile(composePath),
		test.WithName(projectName),
		test.WithCustomLabel(map[string]string{
			DocoCDLabels.Deployment.Name:       projectName,
			DocoCDLabels.Deployment.TargetRef:  "refs/heads/main",
			DocoCDLabels.Deployment.CommitSHA:  commitSHA,
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

func TestRecreateProjectManagedLegacyCheckout(t *testing.T) {
	for _, tc := range []struct {
		name        string
		published   bool
		hostAlias   bool
		hostPath    bool
		running     bool
		unavailable bool
	}{
		{name: "unpublished deployed revision"},
		{name: "already published deployed revision", published: true},
		{name: "auto-detected host path differs from mount path", hostAlias: true},
		{name: "explicit host path differs from mount path", hostAlias: true, hostPath: true},
		{name: "running legacy stack", running: true},
		{name: "deployed revision absent from mirror", unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dataMountPath := t.TempDir()
			repositoryURL := "https://example.com/owner/legacy"
			projectName := test.ConvertTestName(t.Name())
			repoPath := filepath.Join(dataMountPath, git.GetRepoName(repositoryURL))
			composeContent := `services:
  web:
    image: nginx:latest
    labels:
      example.com/source: committed
    volumes:
      - ./nginx/data/custom:/source-config:ro
`
			sourceRepo := createGitRepoWithCompose(t, map[string]string{
				".doco-cd.yml":                       fmt.Sprintf("name: %s\nworking_dir: stack\ncompose_files:\n  - compose.yml\n", projectName),
				"stack/compose.yml":                  composeContent,
				"stack/nginx/data/custom/config.txt": "committed configuration\n",
			})

			sourceWorktree, err := sourceRepo.Worktree()
			if err != nil {
				t.Fatal(err)
			}

			head, err := sourceRepo.Head()
			if err != nil {
				t.Fatal(err)
			}

			commitSHA := head.Hash().String()
			if tc.unavailable {
				commitSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
			}

			// Polling may have advanced the mirror since this stack was deployed.
			// Recreation must still use the recorded commit, not the branch tip.
			if err := os.WriteFile(filepath.Join(sourceWorktree.Filesystem.Root(), "stack", "compose.yml"),
				[]byte(strings.ReplaceAll(composeContent, "committed", "newer")), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := sourceWorktree.Add("stack/compose.yml"); err != nil {
				t.Fatal(err)
			}

			if _, err := sourceWorktree.Commit("advance source", &gogit.CommitOptions{
				Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()},
			}); err != nil {
				t.Fatal(err)
			}

			if _, err := gogit.PlainClone(repoPath, false, &gogit.CloneOptions{URL: sourceWorktree.Filesystem.Root()}); err != nil {
				t.Fatal(err)
			}

			// Startup migration moves .git into mirror but retains the old files
			// while containers still reference their legacy working directory.
			if err := os.Rename(filepath.Join(repoPath, ".git"), filepath.Join(repoPath, store.MirrorSubdir)); err != nil {
				t.Fatal(err)
			}

			gitStore, err := store.NewGitStore(store.GitStoreOptions{CloneURL: repositoryURL, BaseDir: repoPath})
			if err != nil {
				t.Fatal(err)
			}

			if tc.published {
				if _, err := gitStore.Publish(ctx, store.Revision(commitSHA)); err != nil {
					t.Fatal(err)
				}
			}

			hostDataPath := dataMountPath
			if tc.hostAlias {
				hostDataPath = filepath.Join(t.TempDir(), "host-data")
				if err := os.Symlink(dataMountPath, hostDataPath); err != nil {
					t.Fatal(err)
				}
			}

			hostRepoPath := filepath.Join(hostDataPath, git.GetRepoName(repositoryURL))

			legacyComposePath := filepath.Join(hostRepoPath, "stack", "compose.yml")
			if err := os.WriteFile(legacyComposePath, []byte(strings.ReplaceAll(composeContent, "committed", "stale")), 0o600); err != nil {
				t.Fatal(err)
			}

			stack := test.ComposeUp(ctx, t, test.WithFile(legacyComposePath), test.WithName(projectName),
				test.WithCustomLabel(map[string]string{
					DocoCDLabels.Deployment.Name:       projectName,
					DocoCDLabels.Deployment.TargetRef:  "main",
					DocoCDLabels.Deployment.CommitSHA:  commitSHA,
					DocoCDLabels.Deployment.ConfigHash: "deployed-config-hash",
					DocoCDLabels.Source.Type:           string(config.SourceTypeGit),
					DocoCDLabels.Source.Name:           "owner/legacy",
					DocoCDLabels.Source.URL:            repositoryURL,
				}))

			beforeID := stack.ServiceContainerID(ctx, t, "web")
			if !tc.running {
				if err := StopProject(ctx, stack.DockerCli, projectName, 30*time.Second); err != nil {
					t.Fatal(err)
				}
			}

			opts := ScheduledComposeOptions{
				ComposeLoad:         ComposeLoadOptions{DataMountPath: dataMountPath},
				DeployConfigBaseDir: "/",
			}
			if tc.hostPath {
				opts.ComposeLoad.DataHostPath = hostDataPath
			}

			err = RecreateProject(ctx, "", stack.DockerCli, projectName, "", 30*time.Second, nil, opts)
			if tc.unavailable {
				if !errors.Is(err, ErrComposeSourceRevisionConflict) || !errors.Is(err, store.ErrRevisionNotFound) {
					t.Fatalf("RecreateProject() error = %v, want source conflict caused by missing cached revision", err)
				}

				if got := stack.ServiceContainerID(ctx, t, "web"); got != beforeID {
					t.Fatal("recreation replaced a container despite the missing deployed revision")
				}

				return
			}

			if err != nil {
				t.Fatalf("RecreateProject() error = %v", err)
			}

			afterID := stack.ServiceContainerID(ctx, t, "web")
			if afterID == beforeID {
				t.Fatal("legacy container was restarted instead of recreated")
			}

			after, err := stack.Client.ContainerInspect(ctx, afterID, client.ContainerInspectOptions{})
			if err != nil {
				t.Fatal(err)
			}

			artifactRoot := filepath.Join(hostRepoPath, store.ArtifactsSubdir, commitSHA)
			for key, want := range map[string]string{
				api.WorkingDirLabel:                  filepath.Join(artifactRoot, "stack"),
				api.ConfigFilesLabel:                 filepath.Join(artifactRoot, "stack", "compose.yml"),
				DocoCDLabels.Deployment.WorkingDir:   filepath.Join(artifactRoot, "stack"),
				DocoCDLabels.Deployment.CommitSHA:    commitSHA,
				DocoCDLabels.Deployment.ConfigHash:   "deployed-config-hash",
				DocoCDLabels.Source.ConfigRevision:   commitSHA,
				DocoCDLabels.Source.ConfigWorkingDir: artifactRoot,
				"example.com/source":                 "committed",
			} {
				if got := after.Container.Config.Labels[key]; got != want {
					t.Errorf("label %q = %q, want %q", key, got, want)
				}
			}

			wantMount := filepath.Join(artifactRoot, "stack", "nginx", "data", "custom")
			if len(after.Container.Mounts) != 1 || after.Container.Mounts[0].Source != wantMount {
				t.Fatalf("bind mounts = %#v, want source %s", after.Container.Mounts, wantMount)
			}

			if !after.Container.State.Running {
				t.Fatal("recreated container is not running")
			}

			if tc.published {
				if err := RecreateProject(ctx, "", stack.DockerCli, projectName, "web", 30*time.Second, nil, opts); err != nil {
					t.Fatalf("recreating the migrated service again failed: %v", err)
				}
			}
		})
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
			name: "prefers the latest managed deployment",
			containers: []api.ContainerSummary{
				{Labels: map[string]string{
					DocoCDLabels.Deployment.Name:       "managed",
					DocoCDLabels.Source.Name:           "owner/repo",
					DocoCDLabels.Deployment.Timestamp:  "2026-01-01T00:00:00Z",
					DocoCDLabels.Deployment.WorkingDir: "/data/repo/artifacts/old",
				}},
				{Labels: map[string]string{
					DocoCDLabels.Deployment.Name:       "managed",
					DocoCDLabels.Source.Name:           "owner/repo",
					DocoCDLabels.Deployment.Timestamp:  "2026-01-02T00:00:00Z",
					DocoCDLabels.Deployment.WorkingDir: "/data/repo/artifacts/new",
				}},
				{Labels: map[string]string{
					DocoCDLabels.Deployment.Name:       "managed",
					DocoCDLabels.Source.Name:           "owner/repo",
					DocoCDLabels.Deployment.Timestamp:  "invalid",
					DocoCDLabels.Deployment.WorkingDir: "/data/repo/artifacts/invalid",
				}},
			},
			want: map[string]string{
				DocoCDLabels.Deployment.Name:       "managed",
				DocoCDLabels.Source.Name:           "owner/repo",
				DocoCDLabels.Deployment.Timestamp:  "2026-01-02T00:00:00Z",
				DocoCDLabels.Deployment.WorkingDir: "/data/repo/artifacts/new",
			},
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
	t.Run("matching published Git artifact", func(t *testing.T) {
		dataMountPath := t.TempDir()
		ref := composeScheduledServiceRef{
			Project:       "example",
			RepositoryURL: "https://example.com/owner/repo",
			SourceType:    string(config.SourceTypeGit),
		}

		repoPath := filepath.Join(dataMountPath, git.GetRepoName(ref.RepositoryURL))
		commitSHA := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

		// sourceRepoPath is a GitStore base directory: the expected revision
		// is only "cached" if it was already published under artifacts/<sha>.
		artifactDir := filepath.Join(repoPath, "artifacts", commitSHA)
		if err := os.MkdirAll(artifactDir, 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(artifactDir, "compose.yaml"), nil, 0o600); err != nil {
			t.Fatal(err)
		}

		err := validateManagedRecreateRevision(ref, map[string]string{
			DocoCDLabels.Deployment.CommitSHA: commitSHA,
		}, repoPath, config.SourceTypeGit)
		if err != nil {
			t.Fatalf("validateManagedRecreateRevision() error = %v", err)
		}
	})

	t.Run("missing published Git artifact", func(t *testing.T) {
		dataMountPath := t.TempDir()
		ref := composeScheduledServiceRef{
			Project:       "example",
			RepositoryURL: "https://example.com/owner/repo",
			SourceType:    string(config.SourceTypeGit),
		}

		repoPath := filepath.Join(dataMountPath, git.GetRepoName(ref.RepositoryURL))

		err := validateManagedRecreateRevision(ref, map[string]string{
			DocoCDLabels.Deployment.CommitSHA: "deadbeef",
		}, repoPath, config.SourceTypeGit)
		if !errors.Is(err, ErrComposeSourceRevisionConflict) {
			t.Fatalf("validateManagedRecreateRevision() error = %v, want ErrComposeSourceRevisionConflict", err)
		}
	})

	t.Run("missing deployed revision", func(t *testing.T) {
		err := validateManagedRecreateRevision(composeScheduledServiceRef{Project: "example"}, nil, "", config.SourceTypeGit)
		if !errors.Is(err, ErrComposeSourceRevisionConflict) {
			t.Fatalf("validateManagedRecreateRevision() error = %v, want ErrComposeSourceRevisionConflict", err)
		}
	})

	t.Run("matching published OCI artifact", func(t *testing.T) {
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

		// sourcePath is an OCIStore base directory: the expected revision
		// is only "cached" if it was already published under artifacts/<digest>.
		artifactDir := filepath.Join(sourcePath, "artifacts", store.ArtifactDirName("sha256:deployed"))
		if err := os.MkdirAll(artifactDir, 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(artifactDir, "compose.yaml"), nil, 0o600); err != nil {
			t.Fatal(err)
		}

		err = validateManagedRecreateRevision(ref, map[string]string{
			DocoCDLabels.Deployment.CommitSHA: "sha256:deployed",
		}, sourcePath, sourceType)
		if err != nil {
			t.Fatalf("validateManagedRecreateRevision() error = %v", err)
		}
	})

	for _, tc := range []struct {
		name              string
		publishedRevision string
	}{
		{name: "mismatched OCI artifact", publishedRevision: "sha256:newer"},
		{name: "missing OCI artifact"},
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

			if tc.publishedRevision != "" {
				if err := os.MkdirAll(filepath.Join(sourcePath, "artifacts", store.ArtifactDirName(store.Revision(tc.publishedRevision))), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			err = validateManagedRecreateRevision(ref, map[string]string{
				DocoCDLabels.Deployment.CommitSHA: "sha256:deployed",
			}, sourcePath, sourceType)
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
