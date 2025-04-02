package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/kimdre/doco-cd/internal/webhook"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	"github.com/kimdre/doco-cd/internal/config"
)

func createTmpDir(t *testing.T) string {
	dirName, err := os.MkdirTemp(os.TempDir(), "test-*")
	if err != nil {
		t.Fatal(err)
	}

	return dirName
}

func cloneOnDir(path, url, ref string) (err error) {
	_, err = git.PlainClone(path, false, &git.CloneOptions{
		URL:             url,
		SingleBranch:    true,
		ReferenceName:   plumbing.ReferenceName(ref),
		Tags:            git.NoTags,
		Depth:           1,
		InsecureSkipTLS: true,
	})

	return err
}

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

func TestGetSocketGroupOwner(t *testing.T) {
	_, err := GetSocketGroupOwner()
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoadCompose(t *testing.T) {
	ctx := context.Background()

	dirName := createTmpDir(t)
	t.Cleanup(func() {
		err := os.RemoveAll(dirName)
		if err != nil {
			t.Fatal(err)
		}
	})

	filePath := filepath.Join(dirName, "test.compose.yaml")

	createComposeFile(t, filePath, composeContents)

	project, err := LoadCompose(ctx, dirName, projectName, []string{filePath})
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
		os.Setenv("WEBHOOK_SECRET", "notempty")
	}

	value = os.Getenv("GIT_ACCESS_TOKEN")
	if value == "" {
		os.Setenv("GIT_ACCESS_TOKEN", "notempty")
	}

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	p := webhook.ParsedPayload{
		Ref:       "main",
		CommitSHA: "47a1d528f11c4635eaba155c1e6899c6a1af3679",
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

	dirName := createTmpDir(t)
	err = cloneOnDir(dirName, p.CloneURL, p.Ref)
	if err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(dirName, "test.compose.yaml")

	t.Log("Load compose file")
	createComposeFile(t, filePath, composeContents)

	project, err := LoadCompose(ctx, dirName, projectName, []string{filePath})
	if err != nil {
		t.Fatal(err)
	}

	t.Log("Deploy compose")

	dockerCli, err := CreateDockerCli(c.DockerQuietDeploy, !c.SkipTLSVerification)
	if err != nil {
		t.Fatal(err)
	}

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

	filePath = filepath.Join(dirName, fileName)

	err = createTestFile(filePath, deployConfig)
	if err != nil {
		t.Fatal(err)
	}

	deployConfigs, err := config.GetDeployConfigs(dirName, projectName, customTarget)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	for _, deployConf := range deployConfigs {
		t.Logf("Deploying '%s' ...", deployConf.Name)
		err = DeployCompose(ctx, dockerCli, project, deployConf, p)
		if err != nil {
			t.Fatal(err)
		}

		containerID, err := GetContainerID(dockerCli.Client(), deployConf.Name)
		if err != nil {
			t.Fatal(err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			OnCrash(
				dockerCli.Client(),
				containerID,
				func() {
					t.Log("cleaning up path: " + dirName)
					fmt.Println("cleaning up path: " + dirName)
					os.RemoveAll(dirName)
				},
				func(err error) { t.Log("an error occurred cleaning up: " + err.Error()) },
			)
		}()

		t.Log("Finished deployment with no errors")

		output, err := Exec(dockerCli.Client(), "test-test-1", "cat", "usr/share/nginx/html/index.html")
		if err != nil {
			t.Fatal(err)
		}

		if strings.TrimSpace(output) != "Hello world!" {
			t.Fatalf("failed to mount: content of 'html/index.html' not equal to content of 'usr/share/nginx/html/index.html': %s", output)
		}

		t.Log("after running cat command")
	}

	t.Log("before running compose down")
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
