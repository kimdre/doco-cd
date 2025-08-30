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
	"strings"
	"testing"
	"time"

	testCompose "github.com/testcontainers/testcontainers-go/modules/compose"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kimdre/doco-cd/internal/notification"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	"github.com/docker/docker/client"
	"github.com/google/uuid"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func createComposeFile(t *testing.T, filePath, content string) {
	err := os.WriteFile(filePath, []byte(content), filesystem.PermOwner)
	if err != nil {
		t.Fatal(err)
	}
}

func createTestFile(fileName string, content string) error {
	err := os.WriteFile(fileName, []byte(content), filesystem.PermOwner)
	if err != nil {
		return err
	}

	return nil
}

const (
	cloneUrlTest    = "https://github.com/kimdre/doco-cd_tests.git"
	projectName     = "test"
	composeContents = `services:
  test:
    image: nginx:latest
    environment:
      GIT_ACCESS_TOKEN:
      WEBHOOK_SECRET:
      TZ: Europe/Berlin
    ports:
      - "80:80"
    volumes:
      - ./html:/usr/share/nginx/html
`
)

var (
	fileName         = ".doco-cd.yaml"
	reference        = git.MainBranch
	workingDirectory = "."
	composeFiles     = []string{"test.compose.yaml"}
	customTarget     = ""

	deployConfig = fmt.Sprintf(`name: %s
reference: %s
working_dir: %s
force_image_pull: true
force_recreate: true
compose_files:
  - %s
`, projectName, reference, workingDirectory, composeFiles[0])
)

func TestVerifySocketConnection(t *testing.T) {
	err := VerifySocketConnection()
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoadCompose(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.compose.yaml")

	createComposeFile(t, filePath, composeContents)

	project, err := LoadCompose(ctx, tmpDir, projectName, []string{filePath}, []string{})
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
	ctx := context.Background()

	encryption.SetupAgeKeyEnvVar(t)

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	dockerCli, err := CreateDockerCli(c.DockerQuietDeploy, !c.SkipTLSVerification)
	if err != nil {
		t.Fatal(err)
	}

	dockerClient, _ := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)

	SwarmModeEnabled, err = CheckDaemonIsSwarmManager(ctx, dockerCli)
	if err != nil {
		log.Fatalf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	if SwarmModeEnabled {
		t.Skip("Swarm mode is enabled, skipping test")
	}

	p := webhook.ParsedPayload{
		Ref:       git.MainBranch,
		CommitSHA: "4d877107dfa2e3b582bd8f8f803befbd3a1d867e",
		Name:      "test",
		FullName:  "kimdre/doco-cd_tests",
		CloneURL:  git.GetAuthUrl(cloneUrlTest, c.AuthType, c.GitAccessToken),
		Private:   true,
	}

	t.Log("Verify socket connection")

	err = VerifySocketConnection()
	if err != nil {
		t.Fatal(err)
	}

	tmpDir := t.TempDir()

	repo, err := git.CloneRepository(tmpDir, p.CloneURL, p.Ref, c.SkipTLSVerification, c.HttpProxy)
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
	createComposeFile(t, filePath, composeContents)

	project, err := LoadCompose(ctx, tmpDir, projectName, []string{filePath}, []string{})
	if err != nil {
		t.Fatal(err)
	}

	t.Log("Deploy compose")

	filePath = filepath.Join(repoPath, fileName)

	err = createTestFile(filePath, deployConfig)
	if err != nil {
		t.Fatal(err)
	}

	deployConfigs, err := config.GetDeployConfigs(tmpDir, projectName, customTarget, p.Ref)
	if err != nil {
		t.Fatal(err)
	}

	for _, deployConf := range deployConfigs {
		t.Cleanup(func() {
			t.Log("Remove test deployment")

			service := compose.NewComposeService(dockerCli)

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

		jobID := uuid.Must(uuid.NewRandom()).String()

		log := logger.New(slog.LevelInfo)
		jobLog := log.With(slog.String("job_id", jobID))

		metadata := notification.Metadata{
			Repository: p.FullName,
			Stack:      deployConf.Name,
			Revision:   notification.GetRevision(deployConf.Reference, latestCommit),
			JobID:      jobID,
		}

		err = DeployStack(jobLog, repoPath, repoPath, &ctx, &dockerCli, dockerClient, &p, deployConf,
			[]git.ChangedFile{}, latestCommit, "test", "poll", false, metadata)
		if err != nil {
			if errors.Is(err, config.ErrDeprecatedConfig) {
				t.Log(err.Error())
			} else {
				t.Fatalf("failed to deploy stack: %v", err)
			}
		}

		t.Log("Verifying deployment")

		containers, err := GetLabeledContainers(ctx, dockerClient, DocoCDLabels.Metadata.Manager, config.AppName)
		if err != nil {
			t.Fatal(err)
		}

		if len(containers) == 0 {
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

		containers, err = GetLabeledContainers(ctx, dockerClient, DocoCDLabels.Metadata.Manager, config.AppName)
		if err != nil {
			t.Fatal(err)
		}

		if len(containers) != 0 {
			t.Fatalf("expected no labeled containers after destruction, got %d", len(containers))
		}

		t.Log("Finished destroying deployment with no errors")
	}
}

func TestHasChangedConfigs(t *testing.T) {
	testCases := []struct {
		name            string
		oldCommit       string
		newCommit       string
		ExpectedChanges bool
	}{
		{
			name:            "Has changes",
			oldCommit:       "182520d6b0c574c319de69d05ba79858712e335e",
			newCommit:       "87344f0f87250cd2b5d82d2483d3a62ee1d18e93",
			ExpectedChanges: true,
		},
		{
			name:            "Has no changes",
			oldCommit:       "72f1a4e88fdeffec3241d6da2ee19757eee3a0fd",
			newCommit:       "151642a5c4f1b16b543d06c60fa9c95e2c7704a2",
			ExpectedChanges: false,
		},
	}

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	url := git.GetAuthUrl(cloneUrlTest, c.AuthType, c.GitAccessToken)

	tmpDir := t.TempDir()

	repo, err := git.CloneRepository(tmpDir, url, git.MainBranch, c.SkipTLSVerification, c.HttpProxy)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	t.Chdir(tmpDir)

	project, err := LoadCompose(t.Context(), tmpDir, projectName, []string{"docker-compose.yml"}, []string{})
	if err != nil {
		t.Fatalf("Failed to load compose file: %v", err)
	}

	for _, tc := range testCases {
		changedFiles, err := git.GetChangedFilesBetweenCommits(repo, plumbing.NewHash(tc.oldCommit), plumbing.NewHash(tc.newCommit))
		if err != nil {
			t.Fatalf("Failed to get changed files: %v", err)
		}

		if tc.ExpectedChanges && len(changedFiles) == 0 {
			t.Fatalf("Expectec changed files, but found none found")
		}

		hasChanged, err := HasChangedConfigs(changedFiles, project)
		if err != nil {
			t.Fatalf("Failed to check for changed configs: %v", err)
		}

		if !hasChanged && tc.ExpectedChanges {
			t.Error("Expected changed configs, but found none")
		}
	}
}

func TestHasChangedSecrets(t *testing.T) {
	testCases := []struct {
		name            string
		oldCommit       string
		newCommit       string
		ExpectedChanges bool
	}{
		{
			name:            "Has changes",
			oldCommit:       "e4bd98139b81fd80938687edc7f9a1a001654e92",
			newCommit:       "d47101db6f9a07b0d36a6245b257c3690782ae69",
			ExpectedChanges: true,
		},
		{
			name:            "Has no changes",
			oldCommit:       "72f1a4e88fdeffec3241d6da2ee19757eee3a0fd",
			newCommit:       "151642a5c4f1b16b543d06c60fa9c95e2c7704a2",
			ExpectedChanges: false,
		},
	}

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	url := git.GetAuthUrl(cloneUrlTest, c.AuthType, c.GitAccessToken)

	tmpDir := t.TempDir()

	repo, err := git.CloneRepository(tmpDir, url, git.MainBranch, c.SkipTLSVerification, c.HttpProxy)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	t.Chdir(tmpDir)

	project, err := LoadCompose(t.Context(), tmpDir, projectName, []string{"docker-compose.yml"}, []string{})
	if err != nil {
		t.Fatalf("Failed to load compose file: %v", err)
	}

	for _, tc := range testCases {
		changedFiles, err := git.GetChangedFilesBetweenCommits(repo, plumbing.NewHash(tc.oldCommit), plumbing.NewHash(tc.newCommit))
		if err != nil {
			t.Fatalf("Failed to get changed files: %v", err)
		}

		if tc.ExpectedChanges && len(changedFiles) == 0 {
			t.Fatalf("Expectec changed files, but found none found")
		}

		hasChanged, err := HasChangedSecrets(changedFiles, project)
		if err != nil {
			t.Fatalf("Failed to check for changed secrets: %v", err)
		}

		if !hasChanged && tc.ExpectedChanges {
			t.Error("Expected changed secrets, but found none")
		}
	}
}

func TestHasChangedBindMounts(t *testing.T) {
	testCases := []struct {
		name            string
		oldCommit       string
		newCommit       string
		ExpectedChanges bool
	}{
		{
			name:            "Has changes",
			oldCommit:       "72f1a4e88fdeffec3241d6da2ee19757eee3a0fd",
			newCommit:       "151642a5c4f1b16b543d06c60fa9c95e2c7704a2",
			ExpectedChanges: true,
		},
		{
			name:            "Has no changes",
			oldCommit:       "e4bd98139b81fd80938687edc7f9a1a001654e92",
			newCommit:       "d47101db6f9a07b0d36a6245b257c3690782ae69",
			ExpectedChanges: false,
		},
	}

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	url := git.GetAuthUrl(cloneUrlTest, c.AuthType, c.GitAccessToken)

	tmpDir := t.TempDir()

	repo, err := git.CloneRepository(tmpDir, url, git.MainBranch, c.SkipTLSVerification, c.HttpProxy)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	t.Chdir(tmpDir)

	project, err := LoadCompose(t.Context(), tmpDir, projectName, []string{"docker-compose.yml"}, []string{})
	if err != nil {
		t.Fatalf("Failed to load compose file: %v", err)
	}

	for _, tc := range testCases {
		changedFiles, err := git.GetChangedFilesBetweenCommits(repo, plumbing.NewHash(tc.oldCommit), plumbing.NewHash(tc.newCommit))
		if err != nil {
			t.Fatalf("Failed to get changed files: %v", err)
		}

		if tc.ExpectedChanges && len(changedFiles) == 0 {
			t.Fatalf("Expectec changed files, but found none found")
		}

		hasChanged, err := HasChangedBindMounts(changedFiles, project)
		if err != nil {
			t.Fatalf("Failed to check for changed bind mounts: %v", err)
		}

		if !hasChanged && tc.ExpectedChanges {
			t.Error("Expected changed bind mounts, but found none")
		}
	}
}

func startTestContainer(ctx context.Context, t *testing.T) (*testCompose.DockerCompose, error) {
	t.Chdir(t.TempDir())

	stack, err := testCompose.NewDockerComposeWith(
		testCompose.StackIdentifier("test"),
		testCompose.WithStackReaders(strings.NewReader(composeContents)),
	)
	if err != nil {
		t.Fatalf("failed to create stack: %v", err)
	}

	err = stack.
		WaitForService("test", wait.ForListeningPort("80/tcp")).
		Up(ctx, testCompose.Wait(true))
	if err != nil {
		t.Fatalf("failed to start stack: %v", err)
	}

	t.Cleanup(func() {
		err = stack.Down(
			ctx,
			testCompose.RemoveOrphans(true),
			testCompose.RemoveVolumes(true),
			testCompose.RemoveImagesLocal,
		)
		if err != nil {
			t.Fatalf("Failed to stop stack: %v", err)
		}
	})

	return stack, err
}

func TestRestartProject(t *testing.T) {
	ctx := context.Background()

	_, err := startTestContainer(ctx, t)
	if err != nil {
		t.Fatalf("failed to start test container: %v", err)
	}

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

	err = RestartProject(ctx, dockerCli, "test", timeout)
	if err != nil {
		t.Fatalf("failed to restart project: %v", err)
	}
}

func TestStopProject(t *testing.T) {
	ctx := context.Background()

	_, err := startTestContainer(ctx, t)
	if err != nil {
		t.Fatalf("failed to start test container: %v", err)
	}

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

	err = StopProject(ctx, dockerCli, "test", timeout)
	if err != nil {
		t.Fatalf("failed to stop project: %v", err)
	}
}

func TestStartProject(t *testing.T) {
	ctx := context.Background()

	_, err := startTestContainer(ctx, t)
	if err != nil {
		t.Fatalf("failed to start test container: %v", err)
	}

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

	err = StopProject(ctx, dockerCli, "test", timeout)
	if err != nil {
		t.Fatalf("failed to stop project: %v", err)
	}

	time.Sleep(3 * time.Second)

	t.Log("Starting project")

	err = StartProject(ctx, dockerCli, "test", timeout)
	if err != nil {
		t.Fatalf("failed to start project: %v", err)
	}
}

func TestRemoveProject(t *testing.T) {
	ctx := context.Background()

	_, err := startTestContainer(ctx, t)
	if err != nil {
		t.Fatalf("failed to start test container: %v", err)
	}

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	dockerCli, err := CreateDockerCli(c.DockerQuietDeploy, !c.SkipTLSVerification)
	if err != nil {
		t.Fatal(err)
	}

	timeout := time.Duration(30) * time.Second

	t.Log("Removing project")

	err = RemoveProject(ctx, dockerCli, "test", timeout, true, true)
	if err != nil {
		t.Fatalf("failed to remove project: %v", err)
	}

	// Verify project is removed
	containers, err := GetProject(ctx, dockerCli, "test")
	if err != nil {
		t.Fatalf("failed to get project: %v", err)
	}

	if len(containers) != 0 {
		t.Fatalf("expected 0 containers, got %d", len(containers))
	}
}

func TestGetProject(t *testing.T) {
	ctx := context.Background()

	_, err := startTestContainer(ctx, t)
	if err != nil {
		t.Fatalf("failed to start test container: %v", err)
	}

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	dockerCli, err := CreateDockerCli(c.DockerQuietDeploy, !c.SkipTLSVerification)
	if err != nil {
		t.Fatal(err)
	}

	t.Log("Getting project")

	containers, err := GetProject(ctx, dockerCli, "test")
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

	_, err := startTestContainer(ctx, t)
	if err != nil {
		t.Fatalf("failed to start test container: %v", err)
	}

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
