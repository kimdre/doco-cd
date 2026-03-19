package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"

	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"

	"github.com/kimdre/doco-cd/internal/secretprovider"

	"github.com/kimdre/doco-cd/internal/docker/swarm"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func createComposeFile(t *testing.T, filePath, content string) {
	t.Helper()

	err := os.WriteFile(filePath, []byte(content), filesystem.PermOwner)
	if err != nil {
		t.Fatal(err)
	}
}

func createTestFile(t *testing.T, fileName string, content string) error {
	t.Helper()

	err := os.WriteFile(fileName, []byte(content), filesystem.PermOwner)
	if err != nil {
		return err
	}

	return nil
}

const cloneUrlTest = "https://github.com/kimdre/doco-cd_tests.git"

var (
	fileName         = ".doco-cd.yaml"
	reference        = git.MainBranch
	workingDirectory = "."
	composeFiles     = []string{"test.compose.yaml"}
	customTarget     = ""
)

// Helper to generate compose YAML with a random port.
func generateComposeContents() string {
	return `services:
  test:
    image: nginx:latest
    environment:
      TZ: Europe/Berlin
    ports:
      - "80"
    volumes:
      - ./html:/usr/share/nginx/html
`
}

func TestVerifySocketConnection(t *testing.T) {
	t.Parallel()

	err := VerifySocketConnection()
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoadCompose(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tmpDir := t.TempDir()

	filePath := filepath.Join(tmpDir, "test.compose.yaml")

	composeYAML := generateComposeContents()
	createComposeFile(t, filePath, composeYAML)

	stackName := test.ConvertTestName(t.Name())

	project, err := LoadCompose(ctx, tmpDir, tmpDir, stackName, []string{filePath}, []string{".env"}, []string{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}

	serialized, err := json.MarshalIndent(project, "", " ")
	if err != nil {
		t.Error(err.Error())
	}

	t.Log(string(serialized))

	if len(project.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(project.Services))
	}
}

func TestDeployCompose(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	ctx := context.Background()

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	dockerCli, err := CreateDockerCli(c.DockerQuietDeploy, !c.SkipTLSVerification)
	if err != nil {
		t.Fatal(err)
	}

	dockerClient, _ := client.New(
		client.FromEnv,
	)

	secretProvider, err := secretprovider.Initialize(ctx, c.SecretProvider, "v0.0.0-test")
	if err != nil {
		if errors.Is(err, bitwardensecretsmanager.ErrNotSupported) {
			t.Skip(err.Error())
		}

		t.Fatalf("failed to initialize secret provider: %s", err.Error())

		return
	}

	if secretProvider != nil {
		t.Cleanup(func() {
			secretProvider.Close()
		})
	}

	swarm.ModeEnabled, err = swarm.CheckDaemonIsSwarmManager(ctx, dockerCli)
	if err != nil {
		log.Fatalf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	if swarm.ModeEnabled {
		t.Skip("Swarm mode is enabled, skipping test")
	}

	p := webhook.ParsedPayload{
		Ref:       git.MainBranch,
		CommitSHA: "4d877107dfa2e3b582bd8f8f803befbd3a1d867e",
		Name:      uuid.Must(uuid.NewV7()).String(),
		FullName:  "kimdre/doco-cd_tests",
		CloneURL:  cloneUrlTest,
		Private:   true,
	}

	t.Log("Verify socket connection")

	err = VerifySocketConnection()
	if err != nil {
		t.Fatal(err)
	}

	tmpDir := t.TempDir()

	auth, err := git.GetAuthMethod(p.CloneURL, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	if auth != nil {
		t.Logf("Using auth method: %s", auth.Name())
	} else {
		t.Log("No auth method configured, using anonymous access")
	}

	repo, err := git.CloneRepository(tmpDir, p.CloneURL, p.Ref, c.SkipTLSVerification, c.HttpProxy, auth, c.GitCloneSubmodules)
	if err != nil {
		if !errors.Is(err, git.ErrRepositoryAlreadyExists) {
			t.Fatal(err)
		}
	}

	latestCommit, err := git.GetLatestCommit(repo, p.Ref)
	if err != nil {
		t.Fatal(err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	repoPath := worktree.Filesystem.Root()
	filePath := filepath.Join(repoPath, "test.compose.yaml")

	t.Log("Load compose file")

	composeYAML := generateComposeContents()
	createComposeFile(t, filePath, composeYAML)

	stackName := test.ConvertTestName(t.Name())

	project, err := LoadCompose(ctx, tmpDir, tmpDir, stackName, []string{filePath}, []string{}, []string{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}

	t.Log("Deploy compose")

	filePath = filepath.Join(repoPath, fileName)

	deployConfig := fmt.Sprintf(`name: %s
reference: %s
working_dir: %s
force_image_pull: true
force_recreate: true
compose_files:
  - %s
`, stackName, reference, workingDirectory, composeFiles[0])

	err = createTestFile(t, filePath, deployConfig)
	if err != nil {
		t.Fatal(err)
	}

	deployConfigs, err := config.GetDeployConfigs(tmpDir, c.DeployConfigBaseDir, stackName, customTarget, p.Ref)
	if err != nil {
		t.Fatal(err)
	}

	for _, deployConf := range deployConfigs {
		t.Cleanup(func() {
			t.Log("Remove test deployment")

			service, err := compose.NewComposeService(dockerCli)
			if err != nil {
				t.Fatalf("failed to create compose service: %v", err)
			}

			downOpts := api.DownOptions{
				RemoveOrphans: true,
				Images:        "all",
				Volumes:       true,
			}

			err = service.Down(ctx, project.Name, downOpts)
			if err != nil {
				t.Fatal(err)
			}
		})

		t.Logf("Deploying '%s'", deployConf.Name)

		jobID := uuid.Must(uuid.NewV7()).String()

		testLog := logger.New(slog.LevelInfo)
		jobLog := testLog.With(slog.String("job_id", jobID))

		if secretProvider != nil && len(deployConf.ExternalSecrets) > 0 {
			resolvedSecrets, err := secretProvider.ResolveSecretReferences(ctx, deployConf.ExternalSecrets)
			if err != nil {
				t.Fatalf("failed to resolve external secrets: %s", err.Error())
			}

			for k, v := range resolvedSecrets {
				deployConf.Internal.Environment[k] = v
			}
		}

		err = retry.New(
			retry.Attempts(3),
			retry.Delay(1*time.Second),
			retry.RetryIf(func(err error) bool {
				return strings.Contains(err.Error(), "No such image:")
			}),
		).Do(func() error {
			return DeployStack(jobLog, repoPath, &ctx, &dockerCli, dockerClient, &p, deployConf,
				[]git.ChangedFile{}, latestCommit, "dev", false)
		})
		if err != nil {
			t.Fatalf("failed to deploy stack: %v", err)
		}

		t.Log("Verifying deployment")

		serviceLabels, err := GetLabeledServices(ctx, dockerClient, DocoCDLabels.Deployment.Name, deployConf.Name)
		if err != nil {
			t.Fatal(err)
		}

		if len(serviceLabels) == 0 {
			t.Fatal("expected at least one labeled container, got none")
		}

		containerID, err := GetContainerID(dockerCli.Client(), deployConf.Name)
		if err != nil {
			t.Fatal(err)
		}

		if containerID == "" {
			t.Fatal("expected container ID, got empty string")
		}

		t.Log("Finished deployment with no errors")

		mountPoint, err := GetMountPointByDestination(dockerClient, containerID, "/usr/share/nginx/html")
		if err != nil {
			t.Fatal(err)
		}

		if mountPoint.Source != filepath.Join(repoPath, "html") {
			t.Fatalf("failed to mount: source '%s' not equal to destination '%s'", mountPoint.Source, filepath.Join(repoPath, "html"))
		}

		t.Logf("Mount point source: %s, destination: %s", mountPoint.Source, mountPoint.Destination)

		txtOutput, err := Exec(dockerCli.Client(), containerID, "cat", "usr/share/nginx/html/index.html")
		if err != nil {
			t.Fatal(err)
		}

		const expectedString = "Hello world!"

		if strings.TrimSpace(txtOutput) != expectedString {
			t.Fatalf("failed to mount: content of 'html/index.html' not equal to content of 'usr/share/nginx/html/index.html': %s", txtOutput)
		}

		// Get output of web server
		htmlOutput, err := Exec(dockerCli.Client(), containerID, "curl", "localhost")
		if err != nil {
			t.Fatal(err)
		}

		if strings.TrimSpace(htmlOutput) != expectedString {
			t.Fatalf("failed to mount: content of 'html/index.html' not equal to content of 'usr/share/nginx/html/index.html': %s", htmlOutput)
		}

		t.Log("Destroying deployment")

		err = DestroyStack(jobLog, &ctx, &dockerCli, deployConf)
		if err != nil {
			t.Fatal(err)
		}

		t.Log("Verifying destruction")

		serviceLabels, err = GetLabeledServices(ctx, dockerClient, DocoCDLabels.Deployment.Name, deployConf.Name)
		if err != nil {
			t.Fatal(err)
		}

		if len(serviceLabels) != 0 {
			t.Fatalf("expected no labeled containers after destruction, got %d", len(serviceLabels))
		}

		t.Log("Finished destroying deployment with no errors")
	}
}

func TestHasChangedConfigs(t *testing.T) {
	t.Parallel()

	const repoRoot = "/data/doco-cd/fake-repo-root"

	testCases := []struct {
		name            string
		changePath      []string
		project         *types.Project
		ExpectedChanges []string
	}{
		{
			name: "same path in service config and changed files",
			changePath: []string{
				repoRoot + "/test",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						Configs: []types.ServiceConfigObjConfig{
							{
								Source: "test",
							},
						},
					},
				},
				Configs: map[string]types.ConfigObjConfig{
					"test": {
						File: repoRoot + "/test",
					},
				},
			},
			ExpectedChanges: []string{"svc1"},
		},
		{
			name: "parent path in service config and changed in sub files",
			changePath: []string{
				repoRoot + "/test/subdir/config.yaml",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						Configs: []types.ServiceConfigObjConfig{
							{
								Source: "test",
							},
						},
					},
				},
				Configs: map[string]types.ConfigObjConfig{
					"test": {
						File: repoRoot + "/test",
					},
				},
			},
			ExpectedChanges: []string{"svc1"},
		},

		{
			name: "change in different path than service config",
			changePath: []string{
				repoRoot + "/other/subdir/config.yaml",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						Configs: []types.ServiceConfigObjConfig{
							{
								Source: "test",
							},
						},
					},
				},
				Configs: map[string]types.ConfigObjConfig{
					"test": {
						File: repoRoot + "/test",
					},
				},
			},
			ExpectedChanges: []string{},
		},
		{
			name: "change in different path than service config",
			changePath: []string{
				repoRoot + "/other/subdir/config.yaml",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						Configs: []types.ServiceConfigObjConfig{
							{
								Source: "cfg",
							},
							{
								Source: "cfg2",
							},
						},
					},
				},
				Configs: map[string]types.ConfigObjConfig{
					"cfg": {
						File: repoRoot + "/other2/subdir/config.yaml",
					},
					"cfg2": {
						File: repoRoot + "/other2/other/subdir/config.yaml",
					},
				},
			},
			ExpectedChanges: []string{},
		},
		{
			name:       "Has no changes",
			changePath: []string{},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						Configs: []types.ServiceConfigObjConfig{
							{
								Source: "test",
							},
						},
					},
				},
				Configs: map[string]types.ConfigObjConfig{
					"test": {
						File: repoRoot + "/test",
					},
				},
			},
			ExpectedChanges: []string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			changes, err := HasChangedConfigs(tc.changePath, tc.project)
			if err != nil {
				t.Fatalf("Failed to check for changed configs: %v", err)
			}

			slices.Sort(changes)
			slices.Sort(tc.ExpectedChanges)

			if !reflect.DeepEqual(changes, tc.ExpectedChanges) {
				t.Errorf("Expected changes %v, but got %v", tc.ExpectedChanges, changes)
			}
		})
	}
}

func TestHasChangedSecrets(t *testing.T) {
	t.Parallel()

	const repoRoot = "/data/doco-cd/fake-repo-root"

	testCases := []struct {
		name            string
		changePath      []string
		project         *types.Project
		ExpectedChanges []string
	}{
		{
			name: "Has changes",
			changePath: []string{
				repoRoot + "/test",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						Secrets: []types.ServiceSecretConfig{
							{
								Source: "test",
							},
						},
					},
				},
				Secrets: map[string]types.SecretConfig{
					"test": {
						File: repoRoot + "/test",
					},
				},
			},
			ExpectedChanges: []string{"svc1"},
		},
		{
			name: "parent path in service secret and changed in sub files",
			changePath: []string{
				repoRoot + "/test/subdir/config.yaml",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						Secrets: []types.ServiceSecretConfig{
							{
								Source: "test",
							},
						},
					},
				},
				Secrets: map[string]types.SecretConfig{
					"test": {
						File: repoRoot + "/test",
					},
				},
			},
			ExpectedChanges: []string{"svc1"},
		},

		{
			name: "change in different path than service secret",
			changePath: []string{
				repoRoot + "/other/subdir/config.yaml",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						Secrets: []types.ServiceSecretConfig{
							{
								Source: "test",
							},
						},
					},
				},
				Secrets: map[string]types.SecretConfig{
					"test": {
						File: repoRoot + "/test",
					},
				},
			},
			ExpectedChanges: []string{},
		},
		{
			name: "change in different path than service secret",
			changePath: []string{
				repoRoot + "/other/subdir/config.yaml",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						Secrets: []types.ServiceSecretConfig{
							{
								Source: "secret",
							},
							{
								Source: "secret2",
							},
						},
					},
				},
				Secrets: map[string]types.SecretConfig{
					"secret": {
						File: repoRoot + "/other2/subdir/config.yaml",
					},
					"secret2": {
						File: repoRoot + "/other2/other/subdir/config.yaml",
					},
				},
			},
			ExpectedChanges: []string{},
		},
		{
			name:       "Has no changes",
			changePath: []string{},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						Secrets: []types.ServiceSecretConfig{
							{
								Source: "test",
							},
						},
					},
				},
				Secrets: map[string]types.SecretConfig{
					"test": {
						File: repoRoot + "/test",
					},
				},
			},
			ExpectedChanges: []string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			changes, err := HasChangedSecrets(tc.changePath, tc.project)
			if err != nil {
				t.Fatalf("Failed to check for changed secrets: %v", err)
			}

			slices.Sort(changes)
			slices.Sort(tc.ExpectedChanges)

			if !reflect.DeepEqual(changes, tc.ExpectedChanges) {
				t.Errorf("Expected changes %v, but got %v", tc.ExpectedChanges, changes)
			}
		})
	}
}

func TestHasChangedBindMounts(t *testing.T) {
	t.Parallel()

	const repoRoot = "/data/doco-cd/fake-repo-root"

	testCases := []struct {
		name            string
		changePath      []string
		project         *types.Project
		ExpectedChanges []string
	}{
		{
			name: "bind mount changed path are the same",
			changePath: []string{
				repoRoot + "/dir",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						Volumes: []types.ServiceVolumeConfig{
							{
								Type:   "bind",
								Source: repoRoot + "/dir",
							},
						},
					},
				},
			},
			ExpectedChanges: []string{"svc1"},
		},
		{
			name: "same name but different path are different",
			// https://github.com/kimdre/doco-cd/issues/1132
			changePath: []string{
				repoRoot + "/server/cwhc-ser6pro/services-auto/gatus/config.yaml",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"mihomo": {
						Name: "mihomo",
						Volumes: []types.ServiceVolumeConfig{
							{
								Type:   "bind",
								Source: repoRoot + "/server/cwhc-istoreos/services-auto/mihomo/config.yaml",
							},
						},
					},
					"gatus": {
						Name: "gatus",
						Volumes: []types.ServiceVolumeConfig{
							{
								Type:   "bind",
								Source: repoRoot + "/server/cwhc-ser6pro/services-auto/gatus/config.yaml",
							},
						},
					},
				},
			},
			ExpectedChanges: []string{"gatus"},
		},
		{
			name: "bind mount parent path",
			changePath: []string{
				repoRoot + "/a/b/c/d/e/f.txt",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						Volumes: []types.ServiceVolumeConfig{
							{
								Type:   "bind",
								Source: repoRoot + "/a/b/c/d/e/f.txt",
							},
						},
					},
					"svc2": {
						Name: "svc2",
						Volumes: []types.ServiceVolumeConfig{
							{
								Type:   "bind",
								Source: repoRoot + "/a/b/c",
							},
						},
					},
					"svc3": {
						Name: "svc3",
						Volumes: []types.ServiceVolumeConfig{
							{
								Type:   "bind",
								Source: repoRoot + "/b/c/d/e/f.txt",
							},
						},
					},
				},
			},
			ExpectedChanges: []string{"svc1", "svc2"},
		},
		{
			name:       "Has no changes",
			changePath: []string{},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						Volumes: []types.ServiceVolumeConfig{
							{
								Type:   "bind",
								Source: repoRoot + "/dir",
							},
						},
					},
				},
			},
			ExpectedChanges: []string{},
		},
		{
			name: "different path",
			changePath: []string{
				repoRoot + "/dir2",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						Volumes: []types.ServiceVolumeConfig{
							{
								Type:   "bind",
								Source: repoRoot + "/dir",
							},
						},
					},
				},
			},
			ExpectedChanges: []string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			changes, err := HasChangedBindMounts(tc.changePath, tc.project)
			if err != nil {
				t.Fatalf("Failed to check for changed bind mounts: %v", err)
			}

			slices.Sort(changes)
			slices.Sort(tc.ExpectedChanges)

			if !reflect.DeepEqual(changes, tc.ExpectedChanges) {
				t.Errorf("Expected changes %v, but got %v", tc.ExpectedChanges, changes)
			}
		})
	}
}

func TestHasChangedEnvFiles(t *testing.T) {
	t.Parallel()

	const repoRoot = "/data/doco-cd/fake-repo-root"

	testCases := []struct {
		name            string
		changePath      []string
		project         *types.Project
		ExpectedChanges []string
	}{
		{
			name: "same path",
			changePath: []string{
				repoRoot + "/test/.env",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						EnvFiles: []types.EnvFile{
							{
								Path: repoRoot + "/test/.env",
							},
						},
					},
				},
			},
			ExpectedChanges: []string{"svc1"},
		},
		{
			name: "parent path",
			changePath: []string{
				repoRoot + "/test/.env",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						EnvFiles: []types.EnvFile{
							{
								Path: repoRoot + "/test",
							},
						},
					},
				},
			},
			ExpectedChanges: []string{"svc1"},
		},
		{
			name: "different path",
			changePath: []string{
				repoRoot + "/test2/.env",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						EnvFiles: []types.EnvFile{
							{
								Path: repoRoot + "/test/.env",
							},
						},
					},
				},
			},
			ExpectedChanges: []string{},
		},
		{
			name:       "Has no changes",
			changePath: []string{},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name: "svc1",
						EnvFiles: []types.EnvFile{
							{
								Path: repoRoot + "/",
							},
						},
					},
				},
			},
			ExpectedChanges: []string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			changes, err := HasChangedEnvFiles(tc.changePath, tc.project)
			if err != nil {
				t.Fatalf("Failed to check for changed env files: %v", err)
			}

			slices.Sort(changes)
			slices.Sort(tc.ExpectedChanges)

			if !reflect.DeepEqual(changes, tc.ExpectedChanges) {
				t.Errorf("Expected changes %v, but got %v", tc.ExpectedChanges, changes)
			}
		})
	}
}

func TestHasChangedBuildFiles(t *testing.T) {
	t.Parallel()

	const repoRoot = "/data/doco-cd/fake-repo-root"

	project := &types.Project{
		Services: map[string]types.ServiceConfig{
			"svc1": {
				Name: "svc1",
				Build: &types.BuildConfig{
					Context: repoRoot + "/context",
					AdditionalContexts: types.Mapping{
						"dir":  repoRoot + "/additionalCtx/dir",
						"dir2": repoRoot + "/additionalCtx/dir2",
					},
					Dockerfile: repoRoot + "/Dockerfile",
					Secrets: []types.ServiceSecretConfig{
						{
							Source: repoRoot + "/secret",
						},
					},
				},
			},
		},
	}
	testCases := []struct {
		name            string
		changePath      []string
		project         *types.Project
		ExpectedChanges []string
	}{
		{
			name: "no build",
			changePath: []string{
				repoRoot + "/test/.env",
			},
			project: &types.Project{
				Services: map[string]types.ServiceConfig{
					"svc1": {
						Name:  "svc1",
						Build: nil,
					},
				},
			},
			ExpectedChanges: []string{},
		},
		{
			name:            "no change",
			changePath:      []string{},
			project:         project,
			ExpectedChanges: []string{},
		},
		{
			name: "context changed",
			changePath: []string{
				repoRoot + "/context",
			},
			project:         project,
			ExpectedChanges: []string{"svc1"},
		},
		{
			name: "different path",
			changePath: []string{
				repoRoot + "/context2",
			},
			project:         project,
			ExpectedChanges: []string{},
		},
		{
			name: "additional context changed",
			changePath: []string{
				repoRoot + "/additionalCtx/dir",
			},
			project:         project,
			ExpectedChanges: []string{"svc1"},
		},
		{
			name: "additional context sub dir changed",
			changePath: []string{
				repoRoot + "/additionalCtx/dir/aaa.txt",
			},
			project:         project,
			ExpectedChanges: []string{"svc1"},
		},
		{
			name: "dockerfile changed",
			changePath: []string{
				repoRoot + "/Dockerfile",
			},
			project:         project,
			ExpectedChanges: []string{"svc1"},
		},
		{
			name: "build secret changed",
			changePath: []string{
				repoRoot + "/secret",
			},
			project:         project,
			ExpectedChanges: []string{"svc1"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			changes, err := HasChangedBuildFiles(tc.changePath, tc.project)
			if err != nil {
				t.Fatalf("Failed to check for changed env files: %v", err)
			}

			slices.Sort(changes)
			slices.Sort(tc.ExpectedChanges)

			if !reflect.DeepEqual(changes, tc.ExpectedChanges) {
				t.Errorf("Expected changes %v, but got %v", tc.ExpectedChanges, changes)
			}
		})
	}
}

func TestProjectFilesHaveChanges(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	testCases := []struct {
		name            string
		oldCommit       string
		newCommit       string
		expectedChanges []Change
	}{
		{
			name:      "Changed dotenv in single service",
			oldCommit: "1a62190db8ea6f700dbc6364ea94522c21c3642f",
			newCommit: "268aa233f1dbac17cc521883f7e3755bc37f70c1",
			expectedChanges: []Change{
				{Type: "envFiles", Services: []string{"test"}},
			},
		},
		{
			name:      "Changed bind mount in single service",
			oldCommit: "49913446ea803cb9ecaa441118e0b9ef48e77cc2",
			newCommit: "1a62190db8ea6f700dbc6364ea94522c21c3642f",
			expectedChanges: []Change{
				{Type: "bindMounts", Services: []string{"test"}},
			},
		},
		{
			name:      "Changed secret in single service",
			oldCommit: "b181a3a84af166639c8789bb6be15a6a5bdfe7af",
			newCommit: "1dbfb46f4b3479a8986f1d46fc5d9948dcac2ede",
			expectedChanges: []Change{
				{Type: "secrets", Services: []string{"test"}},
			},
		},
		{
			name:      "Changed config in single service",
			oldCommit: "f0582842c7526c1815a2f4aa46a88301567809bf",
			newCommit: "204ef121cf41686515977e7bc9d33376661252d6",
			expectedChanges: []Change{
				{Type: "configs", Services: []string{"test"}},
			},
		},
		{
			name:            "Changes in Compose Project",
			oldCommit:       "1dbfb46f4b3479a8986f1d46fc5d9948dcac2ede",
			newCommit:       "c50cc1d8cbad3b6e6f2c058cf7a4898a6ba908e6",
			expectedChanges: nil,
		},
		{
			name:      "Changes in Secret and Bind Mount in single service",
			oldCommit: "268aa233f1dbac17cc521883f7e3755bc37f70c1",
			newCommit: "0eb919b35ff46af6efc02425588622adf448a4d4",
			expectedChanges: []Change{
				{Type: "secrets", Services: []string{"test"}},
				{Type: "bindMounts", Services: []string{"test"}},
			},
		},
		{
			name:      "Changes in Bind Mount and Compose Project in Multiple Services",
			oldCommit: "29993e4b57ab55a687f69c1cca945b6c8966806a",
			newCommit: "5327b541f9917213b0b2ca4549f6314c78513bc7",
			expectedChanges: []Change{
				{Type: "bindMounts", Services: []string{"test"}},
			},
		},
		{
			name:      "Changes in Multiple Configs for Multiple Services",
			oldCommit: "5519af2e6ca9ee6a6d751c09290387aa8e317386",
			newCommit: "3af5b37a662eebb0ce2b9006ea69009f726b2789",
			expectedChanges: []Change{
				{Type: "configs", Services: []string{"included", "test"}},
			},
		},
	}

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	auth, err := git.GetAuthMethod(cloneUrlTest, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	if auth != nil {
		t.Logf("Using auth method: %s", auth.Name())
	} else {
		t.Log("No auth method configured, using anonymous access")
	}

	tmpDir := t.TempDir()

	repo, err := git.CloneRepository(tmpDir, cloneUrlTest, git.MainBranch, c.SkipTLSVerification, c.HttpProxy, auth, c.GitCloneSubmodules)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err = git.CheckoutRepository(repo, tc.newCommit, auth, c.GitCloneSubmodules)
			if err != nil {
				t.Fatalf("Failed to checkout old commit: %v", err)
			}

			deployConfigs, err := config.GetDeployConfigs(tmpDir, ".", t.Name(), "", "")
			if err != nil {
				t.Fatal(err)
			}

			d := deployConfigs[0]
			d.Name = test.ConvertTestName(t.Name())

			changedFiles, err := git.GetChangedFilesBetweenCommits(repo, plumbing.NewHash(tc.oldCommit), plumbing.NewHash(tc.newCommit))
			if err != nil {
				t.Fatalf("Failed to get changed files: %v", err)
			}

			project, err := LoadCompose(t.Context(), tmpDir, tmpDir, d.Name, d.ComposeFiles, d.EnvFiles, d.Profiles, map[string]string{})
			if err != nil {
				t.Fatalf("Failed to load compose file: %v", err)
			}

			changes, err := ProjectFilesHaveChanges(tmpDir, changedFiles, project)
			if err != nil {
				t.Fatalf("Failed to get project changes: %v", err)
			}

			sortChanges(tc.expectedChanges)

			if !reflect.DeepEqual(changes, tc.expectedChanges) {
				t.Fatalf("Expected changes: %v, but got: %v", tc.expectedChanges, changes)
			}
		})
	}
}

// TestInjectSecretsToProject tests resolving and injecting secrets from external secret managers into a Docker Compose project.
func TestInjectSecretsToProject(t *testing.T) {
	t.Parallel()

	const (
		varName         = "TEST_PASSWORD"
		composeContents = `services:
  test:
    image: nginx:latest
    environment:
      MY_PASSWORD: "x${TEST_PASSWORD}x"
      IGNORED: "$$NOT_A_SECRET"
    labels:
      MY_LABEL: injected.${TEST_PASSWORD}
`
	)

	// errorCases defines which step should produce an error
	type errorCases struct {
		initialization   bool
		secretResolution bool
	}

	testCases := []struct {
		name                 string
		secretProvider       string
		externalSecrets      map[string]string
		expectedSecretValues map[string]string
		expectError          errorCases
		expectedEnvironment  map[string]string
		expectedLabels       map[string]string
	}{
		{
			name:           "Bitwarden Secrets Manager with correct UUID",
			secretProvider: bitwardensecretsmanager.Name,
			externalSecrets: map[string]string{
				varName: "138e3697-ed58-431c-b866-b3550066343a",
			},
			expectedSecretValues: map[string]string{
				varName: "secret007!",
			},
			expectedEnvironment: map[string]string{
				"MY_PASSWORD": "xsecret007!x",
				"IGNORED":     "$NOT_A_SECRET",
			},
			expectedLabels: map[string]string{
				"MY_LABEL": "injected.secret007!",
			},
		},
		{
			name:           "Bitwarden Secrets Manager with incorrect UUID",
			secretProvider: bitwardensecretsmanager.Name,
			externalSecrets: map[string]string{
				varName: "138e3697-ed58-431c-b866-b35500663dddd",
			},
			expectedSecretValues: map[string]string{
				varName: "secret007!",
			},
			expectError: errorCases{
				secretResolution: true,
			},
		},
		// Disabled because I don't have a 1Password account to test with
		//{
		//	name: "Test with 1Password",
		//  secretProvider: onepassword.Name,
		//	externalSecrets: map[string]string{
		//		varName: "op://DocoCD Tests/Secret/Test Password",
		//	},
		//	expectedSecretValues: map[string]string{
		//		varName: "secret007!",
		//	},
		// },
	}

	ctx := t.Context()

	tmpDir := t.TempDir()

	filePath := filepath.Join(tmpDir, "test.compose.yaml")
	createComposeFile(t, filePath, composeContents)

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if c.SecretProvider != tc.secretProvider {
				t.Skip("Skipping test because secret provider is not configured in app config")
			}

			secretProvider, err := secretprovider.Initialize(ctx, c.SecretProvider, "v0.0.0-test")
			if err != nil {
				if tc.expectError.initialization {
					t.Logf("expected initialization error: %s", err.Error())

					return
				}

				if errors.Is(err, bitwardensecretsmanager.ErrNotSupported) {
					t.Skip(err.Error())
				}

				t.Fatalf("failed to initialize secret provider: %s", err.Error())

				return
			}

			if secretProvider != nil {
				t.Cleanup(func() {
					secretProvider.Close()
				})
			}

			// Resolve external secrets
			resolvedSecrets, err := secretProvider.ResolveSecretReferences(ctx, tc.externalSecrets)
			if err != nil {
				if tc.expectError.secretResolution {
					t.Logf("expected retrieval error: %s", err.Error())

					return
				}

				t.Fatalf("failed to resolve external secrets: %s", err.Error())
			}

			t.Log("Resolved secrets:", resolvedSecrets)

			project, err := LoadCompose(ctx, tmpDir, tmpDir, test.ConvertTestName(t.Name()), []string{filePath}, []string{".env"}, []string{}, resolvedSecrets)
			if err != nil {
				t.Fatal(err)
			}

			for _, service := range project.Services {
				for k, v := range tc.expectedLabels {
					if service.Labels[k] != v {
						t.Fatalf("expected label '%s' to be '%s', got '%s'", k, v, service.Labels[k])
					}

					t.Log("Found label:", service.Labels[k])
				}

				if service.Environment == nil {
					t.Fatal("expected environment variables, got nil")
				}

				for k, v := range tc.expectedEnvironment {
					if *service.Environment[k] != v {
						t.Fatalf("expected environment variable '%s' to be '%s', got '%s'", k, v, *service.Environment[k])
					}

					t.Log("Found environment variable:", *service.Environment[k])
				}

				for k, v := range tc.expectedSecretValues {
					for _, envVal := range service.Environment {
						if strings.Contains(*envVal, v) {
							t.Logf("Secret value for '%s' successfully injected into environment variable", k)

							break
						}
					}
				}
			}
		})
	}
}

func TestRestartProject(t *testing.T) {
	ctx := context.Background()

	test.ComposeUp(ctx, t, test.WithYAML(generateComposeContents()))

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	dockerCli, err := CreateDockerCli(c.DockerQuietDeploy, !c.SkipTLSVerification)
	if err != nil {
		t.Fatal(err)
	}

	timeout := time.Duration(30) * time.Second

	t.Log("Restarting project")

	err = RestartProject(ctx, dockerCli, test.ConvertTestName(t.Name()), timeout)
	if err != nil {
		t.Fatalf("failed to restart project: %v", err)
	}
}

func TestStopProject(t *testing.T) {
	ctx := context.Background()

	test.ComposeUp(ctx, t, test.WithYAML(generateComposeContents()))

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	dockerCli, err := CreateDockerCli(c.DockerQuietDeploy, !c.SkipTLSVerification)
	if err != nil {
		t.Fatal(err)
	}

	timeout := time.Duration(30) * time.Second

	t.Log("Stopping project")

	err = StopProject(ctx, dockerCli, test.ConvertTestName(t.Name()), timeout)
	if err != nil {
		t.Fatalf("failed to stop project: %v", err)
	}
}

func TestStartProject(t *testing.T) {
	ctx := context.Background()

	test.ComposeUp(ctx, t, test.WithYAML(generateComposeContents()))

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	dockerCli, err := CreateDockerCli(c.DockerQuietDeploy, !c.SkipTLSVerification)
	if err != nil {
		t.Fatal(err)
	}

	timeout := time.Duration(30) * time.Second
	stackName := test.ConvertTestName(t.Name())

	t.Log("Stopping project")

	err = StopProject(ctx, dockerCli, stackName, timeout)
	if err != nil {
		t.Fatalf("failed to stop project: %v", err)
	}

	time.Sleep(3 * time.Second)

	t.Log("Starting project")

	err = StartProject(ctx, dockerCli, stackName, timeout)
	if err != nil {
		t.Fatalf("failed to start project: %v", err)
	}
}

func TestRemoveProject(t *testing.T) {
	ctx := context.Background()

	test.ComposeUp(ctx, t, test.WithYAML(generateComposeContents()))

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	dockerCli, err := CreateDockerCli(c.DockerQuietDeploy, !c.SkipTLSVerification)
	if err != nil {
		t.Fatal(err)
	}

	timeout := time.Duration(30) * time.Second
	stackName := test.ConvertTestName(t.Name())

	t.Log("Removing project")

	err = RemoveProject(ctx, dockerCli, stackName, timeout, true, true)
	if err != nil {
		t.Fatalf("failed to remove project: %v", err)
	}

	// Verify project is removed
	containers, err := GetProjectContainers(ctx, dockerCli, stackName)
	if err != nil {
		t.Fatalf("failed to get project: %v", err)
	}

	if len(containers) != 0 {
		t.Fatalf("expected 0 containers, got %d", len(containers))
	}
}

func TestGetProject(t *testing.T) {
	ctx := context.Background()

	test.ComposeUp(ctx, t, test.WithYAML(generateComposeContents()))

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	dockerCli, err := CreateDockerCli(c.DockerQuietDeploy, !c.SkipTLSVerification)
	if err != nil {
		t.Fatal(err)
	}

	t.Log("Getting project")

	containers, err := GetProjectContainers(ctx, dockerCli, test.ConvertTestName(t.Name()))
	if err != nil {
		t.Fatalf("failed to get project: %v", err)
	}

	if len(containers) == 0 {
		t.Fatal("expected at least 1 container, got 0")
	}

	t.Logf("Found %d containers", len(containers))
}

func TestGetProjects(t *testing.T) {
	ctx := context.Background()

	test.ComposeUp(ctx, t, test.WithYAML(generateComposeContents()))

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	dockerCli, err := CreateDockerCli(c.DockerQuietDeploy, !c.SkipTLSVerification)
	if err != nil {
		t.Fatal(err)
	}

	t.Log("Getting projects")

	projects, err := GetProjects(ctx, dockerCli, true)
	if err != nil {
		t.Fatalf("failed to get projects: %v", err)
	}

	if len(projects) == 0 {
		t.Fatal("expected at least 1 project, got 0")
	}

	t.Logf("Found %d projects", len(projects))
}

func Test_checkPathAffected(t *testing.T) {
	const repoRoot = "/data/reporoot"

	tests := []struct {
		name    string
		used    string
		changed string
		want    bool
	}{
		{
			name:    "used end with /",
			used:    repoRoot + "/a/b/",
			changed: repoRoot + "/a/b/c.txt",
			want:    true,
		},
		{
			name:    "used not end with /",
			used:    repoRoot + "/a/b",
			changed: repoRoot + "/a/b/c.txt",
			want:    true,
		},
		{
			name:    "used are prefix of changed",
			used:    repoRoot + "/a/b",
			changed: repoRoot + "/a/b2",
			want:    false,
		},
		{
			name:    "used are prefix of changed and subdir",
			used:    repoRoot + "/a/b",
			changed: repoRoot + "/a/b2/c/d/e.txt",
			want:    false,
		},
		{
			name:    "used are prefix of changed but end with /",
			used:    repoRoot + "/a/b/",
			changed: repoRoot + "/a/b2",
			want:    false,
		},
		{
			name:    "different path /",
			used:    repoRoot + "/a/b/",
			changed: repoRoot + "/c/d",
			want:    false,
		},
		{
			name:    "different path but same suffix",
			used:    repoRoot + "/a/b/e/f/g.txt",
			changed: repoRoot + "/c/d/e/f/g.txt",
			want:    false,
		},
		{
			name:    "file same path",
			used:    repoRoot + "/test.txt",
			changed: repoRoot + "/test.txt",
			want:    true,
		},
		{
			name:    "directory used",
			used:    repoRoot + "/html",
			changed: repoRoot + "/html/index.html",
			want:    true,
		},
		{
			name:    "different path",
			used:    repoRoot + "/html",
			changed: repoRoot + "/configs/test.conf",
			want:    false,
		},
		{
			name:    "different path 2",
			used:    repoRoot + "/html",
			changed: repoRoot + "README.md",
			want:    false,
		},

		{
			name:    "used in subdirectory",
			used:    repoRoot + "/app/html",
			changed: repoRoot + "/app/html/index.html",
			want:    true,
		},
		{
			name:    "used in subdirectory 2",
			used:    repoRoot + "/app/html",
			changed: repoRoot + "/app/configs/test.conf",
			want:    false,
		},
		{
			name:    "no changes in directories",
			used:    repoRoot + "/html",
			changed: repoRoot + "/docs/guide.md",
			want:    false,
		},
		{
			name:    "no changes in directories 2",
			used:    repoRoot + "/html",
			changed: repoRoot + "/configs/test.conf",
			want:    false,
		},
		{
			name:    "no changes in files",
			used:    repoRoot + "/test.txt",
			changed: repoRoot + "/README.md",
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkPathAffected(tt.changed, tt.used)
			if tt.want != got {
				t.Errorf("checkPathAffected(used=%q, changed=%q) = %v, want %v", tt.used, tt.changed, got, tt.want)
			}
		})
	}
}
