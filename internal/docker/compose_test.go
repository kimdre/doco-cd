package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/client"

	"github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/webhook"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	"github.com/kimdre/doco-cd/internal/config"
)

func createComposeFile(t *testing.T, filePath, content string) {
	err := os.WriteFile(filePath, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func createTestFile(fileName string, content string) error {
	err := os.WriteFile(fileName, []byte(content), 0o600)
	if err != nil {
		return err
	}

	return nil
}

var (
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
	value := os.Getenv("WEBHOOK_SECRET")
	if value == "" {
		err := os.Setenv("WEBHOOK_SECRET", "notempty")
		if err != nil {
			t.Fatal(err)
		}
	}

	value = os.Getenv("GIT_ACCESS_TOKEN")
	if value == "" {
		err := os.Setenv("GIT_ACCESS_TOKEN", "notempty")
		if err != nil {
			t.Fatal(err)
		}
	}

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	p := webhook.ParsedPayload{
		Ref:       "main",
		CommitSHA: "4d877107dfa2e3b582bd8f8f803befbd3a1d867e",
		Name:      "test",
		FullName:  "kimdre/test",
		CloneURL:  fmt.Sprintf("https://kimdre:%s@github.com/kimdre/test.git", c.GitAccessToken),
		Private:   true,
	}

	t.Log("Verify socket connection")

	err = VerifySocketConnection()
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	tmpDir := t.TempDir()

	repo, err := git.CloneRepository(tmpDir, p.CloneURL, p.Ref, c.SkipTLSVerification)
	if err != nil {
		if !errors.Is(err, git.ErrRepositoryAlreadyExists) {
			t.Fatal(err)
		}
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

	fileName := ".doco-cd.yaml"
	reference := "refs/heads/main"
	workingDirectory := "/test"
	composeFiles := []string{"test.compose.yaml"}
	customTarget := ""

	deployConfig := fmt.Sprintf(`name: %s
reference: %s
working_dir: %s
force_image_pull: true
force_recreate: true
compose_files:
  - %s
`, projectName, reference, workingDirectory, composeFiles[0])

	filePath = filepath.Join(repoPath, fileName)

	err = createTestFile(filePath, deployConfig)
	if err != nil {
		t.Fatal(err)
	}

	deployConfigs, err := config.GetDeployConfigs(tmpDir, projectName, customTarget)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	for _, deployConf := range deployConfigs {
		t.Logf("Deploying '%s' ...", deployConf.Name)

		err = DeployCompose(ctx, dockerCli, project, deployConf, p, tmpDir)
		if err != nil {
			t.Fatal(err)
		}

		containers, err := GetLabeledContainers(ctx, dockerClient, DocoCDLabels.Metadata.Manager, "doco-cd")
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

		wg.Add(1)

		go func() {
			defer wg.Done()

			OnCrash(
				dockerCli.Client(),
				containerID,
				func() {
					t.Log("cleaning up path: " + tmpDir)
					fmt.Println("cleaning up path: " + tmpDir)
					_ = os.RemoveAll(tmpDir)
				},
				func(err error) { t.Log("an error occurred cleaning up: " + err.Error()) },
			)
		}()

		t.Log("Finished deployment with no errors")

		mountPoint, err := GetMountPointByDestination(dockerClient, containerID, "/usr/share/nginx/html")
		if err != nil {
			t.Fatal(err)
		}

		if mountPoint.Source != filepath.Join(repoPath, "html") {
			t.Fatalf("failed to mount: source '%s' not equal to destination '%s'", mountPoint.Source, filepath.Join(repoPath, "html"))
		}

		t.Logf("Mount point source: %s, destination: %s", mountPoint.Source, mountPoint.Destination)

		output, err := Exec(dockerCli.Client(), containerID, "cat", "usr/share/nginx/html/index.html")
		if err != nil {
			t.Fatal(err)
		}

		if strings.TrimSpace(output) != "Hello world!" {
			t.Fatalf("failed to mount: content of 'html/index.html' not equal to content of 'usr/share/nginx/html/index.html': %s", output)
		}
	}

	service := compose.NewComposeService(dockerCli)

	// Remove test container after test
	downOpts := api.DownOptions{
		RemoveOrphans: true,
		Images:        "all",
		Volumes:       true,
	}

	t.Log("Remove test container")

	err = service.Down(ctx, project.Name, downOpts)
	if err != nil {
		t.Fatal(err)
	}

	wg.Wait()
}
