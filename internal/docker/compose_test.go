package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-git/go-git/v5/plumbing"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	"github.com/docker/docker/client"
	"github.com/google/uuid"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/utils"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func createComposeFile(t *testing.T, filePath, content string) {
	err := os.WriteFile(filePath, []byte(content), utils.PermOwner)
	if err != nil {
		t.Fatal(err)
	}
}

func createTestFile(fileName string, content string) error {
	err := os.WriteFile(fileName, []byte(content), utils.PermOwner)
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

	project, err := LoadCompose(ctx, tmpDir, projectName, []string{filePath})
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

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	p := webhook.ParsedPayload{
		Ref:       "main",
		CommitSHA: "4d877107dfa2e3b582bd8f8f803befbd3a1d867e",
		Name:      "test",
		FullName:  "kimdre/doco-cd_tests",
		CloneURL:  fmt.Sprintf("https://kimdre:%s@github.com/kimdre/doco-cd_tests.git", c.GitAccessToken),
		Private:   true,
	}

	t.Log("Verify socket connection")

	err = VerifySocketConnection()
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

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

	project, err := LoadCompose(ctx, tmpDir, projectName, []string{filePath})
	if err != nil {
		t.Fatal(err)
	}

	t.Log("Deploy compose")

	dockerCli, err := CreateDockerCli(c.DockerQuietDeploy, !c.SkipTLSVerification)
	if err != nil {
		t.Fatal(err)
	}

	dockerClient, _ := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)

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

		err = DeployStack(jobLog, repoPath, repoPath, &ctx, &dockerCli, &p, deployConf, []git.ChangedFile{}, latestCommit, "test", false)
		if err != nil {
			if errors.Is(err, config.ErrDeprecatedConfig) {
				t.Log(err.Error())
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
	var (
		oldCommit = plumbing.NewHash("182520d6b0c574c319de69d05ba79858712e335e")
		newCommit = plumbing.NewHash("87344f0f87250cd2b5d82d2483d3a62ee1d18e93")
	)

	tmpDir := t.TempDir()

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Failed to change to tmpDir: %v", err)
	}

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	url := git.GetAuthUrl(cloneUrlTest, c.AuthType, c.GitAccessToken)

	repo, err := git.CloneRepository(tmpDir, url, git.MainBranch, c.SkipTLSVerification, c.HttpProxy)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	project, err := LoadCompose(t.Context(), tmpDir, projectName, []string{"docker-compose.yml"})
	if err != nil {
		t.Fatalf("Failed to load compose file: %v", err)
	}

	changedFiles, err := git.GetChangedFilesBetweenCommits(repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Failed to get changed files: %v", err)
	}

	if len(changedFiles) == 0 {
		t.Fatal("No changed files found, but expected some")
	}

	hasChanged, err := HasChangedConfigs(changedFiles, project)
	if err != nil {
		t.Fatalf("Failed to check for changed configs: %v", err)
	}

	if !hasChanged {
		t.Error("Expected changed configs, but found none")
	}
}

func TestHasChangedSecrets(t *testing.T) {
	var (
		oldCommit = plumbing.NewHash("e4bd98139b81fd80938687edc7f9a1a001654e92")
		newCommit = plumbing.NewHash("d47101db6f9a07b0d36a6245b257c3690782ae69")
	)

	tmpDir := t.TempDir()

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Failed to change to tmpDir: %v", err)
	}

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	url := git.GetAuthUrl(cloneUrlTest, c.AuthType, c.GitAccessToken)

	repo, err := git.CloneRepository(tmpDir, url, git.MainBranch, c.SkipTLSVerification, c.HttpProxy)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	project, err := LoadCompose(t.Context(), tmpDir, projectName, []string{"docker-compose.yml"})
	if err != nil {
		t.Fatalf("Failed to load compose file: %v", err)
	}

	changedFiles, err := git.GetChangedFilesBetweenCommits(repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Failed to get changed files: %v", err)
	}

	if len(changedFiles) == 0 {
		t.Fatal("No changed files found, but expected some")
	}

	hasChanged, err := HasChangedSecrets(changedFiles, project)
	if err != nil {
		t.Fatalf("Failed to check for changed secrets: %v", err)
	}

	if !hasChanged {
		t.Error("Expected changed secrets, but found none")
	}
}

func TestHasChangedBindMounts(t *testing.T) {
	var (
		oldCommit = plumbing.NewHash("72f1a4e88fdeffec3241d6da2ee19757eee3a0fd")
		newCommit = plumbing.NewHash("151642a5c4f1b16b543d06c60fa9c95e2c7704a2")
	)

	tmpDir := t.TempDir()

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Failed to change to tmpDir: %v", err)
	}

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	url := git.GetAuthUrl(cloneUrlTest, c.AuthType, c.GitAccessToken)

	repo, err := git.CloneRepository(tmpDir, url, git.MainBranch, c.SkipTLSVerification, c.HttpProxy)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	project, err := LoadCompose(t.Context(), tmpDir, projectName, []string{"docker-compose.yml"})
	if err != nil {
		t.Fatalf("Failed to load compose file: %v", err)
	}

	changedFiles, err := git.GetChangedFilesBetweenCommits(repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Failed to get changed files: %v", err)
	}

	if len(changedFiles) == 0 {
		t.Fatal("No changed files found, but expected some")
	}

	hasChanged, err := HasChangedBindMounts(changedFiles, project)
	if err != nil {
		t.Fatalf("Failed to check for changed bind mounts: %v", err)
	}

	if !hasChanged {
		t.Error("Expected changed bind mounts, but found none")
	}
}
