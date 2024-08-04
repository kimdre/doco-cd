package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	"github.com/kimdre/docker-compose-webhook/internal/config"
)

func createTmpDir(t *testing.T) string {
	dirName, err := os.MkdirTemp(os.TempDir(), "test-*")
	if err != nil {
		t.Fatal(err)
	}

	return dirName
}

func createComposeFile(t *testing.T, filePath, content string) {
	err := os.WriteFile(filePath, []byte(content), 0o644)
	if err != nil {
		t.Fatal(err)
	}
}

func createTestFile(fileName string, content string) error {
	err := os.WriteFile(fileName, []byte(content), 0o644)
	if err != nil {
		return err
	}

	return nil
}

var (
	dockerAPIVersion = "1.40"
	projectName      = "test"
	composeContents  = `services:
  test:
    image: nginx:latest
    environment:
      TZ: Europe/Berlin
`
)

func TestVerifySocketConnection(t *testing.T) {
	err := VerifySocketConnection(dockerAPIVersion)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoadCompose(t *testing.T) {
	ctx := context.Background()

	dirName := createTmpDir(t)
	defer func(path string) {
		err := os.RemoveAll(path)
		if err != nil {
			t.Fatal(err)
		}
	}(dirName)

	filePath := filepath.Join(dirName, "test.compose.yaml")

	t.Log("Load compose file")
	createComposeFile(t, filePath, composeContents)

	project, err := LoadCompose(ctx, dirName, projectName, []string{filePath})
	if err != nil {
		t.Fatal(err)
	}

	if len(project.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(project.Services))
	}
}

func TestDeployCompose(t *testing.T) {
	t.Log("Verify socket connection")

	err := VerifySocketConnection(dockerAPIVersion)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	dirName := createTmpDir(t)
	defer func(path string) {
		err := os.RemoveAll(path)
		if err != nil {
			t.Fatal(err)
		}
	}(dirName)

	filePath := filepath.Join(dirName, "test.compose.yaml")

	t.Log("Load compose file")
	createComposeFile(t, filePath, composeContents)

	project, err := LoadCompose(ctx, dirName, projectName, []string{filePath})
	if err != nil {
		t.Fatal(err)
	}

	t.Log("Deploy compose")

	dockerCli, err := CreateDockerCli()
	if err != nil {
		t.Fatal(err)
	}

	fileName := ".compose-deploy.yaml"
	reference := "refs/heads/test"
	workingDirectory := "/test"
	composeFiles := []string{"test.compose.yaml"}

	deployConfig := fmt.Sprintf(`name: %s
reference: %s
working_dir: %s
compose_files:
  - %s
`, projectName, reference, workingDirectory, composeFiles[0])

	filePath = filepath.Join(dirName, fileName)

	err = createTestFile(filePath, deployConfig)
	if err != nil {
		t.Fatal(err)
	}

	deployConf, err := config.GetDeployConfig(dirName, projectName)
	if err != nil {
		t.Fatal(err)
	}

	service := compose.NewComposeService(dockerCli)

	// Remove test container after test
	defer func() {
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
	}()

	err = DeployCompose(ctx, dockerCli, project, deployConf)
	if err != nil {
		t.Fatal(err)
	}
}