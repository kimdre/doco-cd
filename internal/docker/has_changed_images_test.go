package docker

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	testCompose "github.com/testcontainers/testcontainers-go/modules/compose"
	"github.com/testcontainers/testcontainers-go/wait"
)

func cleanupComposeProjectContainers(ctx context.Context, t *testing.T, dockerCli command.Cli, projectName string) {
	t.Helper()

	// 1) Remove containers that still have the compose project label
	args := filters.NewArgs(filters.Arg("label", fmt.Sprintf("%s=%s", api.ProjectLabel, projectName)))

	containersByLabel, err := dockerCli.Client().ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err == nil {
		for _, c := range containersByLabel {
			_ = dockerCli.Client().ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
		}
	}

	// 2) Also remove any leftover containers whose name matches the compose-generated prefix
	// (handles partial/failed runs where labels might be missing).
	namePrefix := "/" + projectName + "-"

	allContainers, err := dockerCli.Client().ContainerList(ctx, container.ListOptions{All: true})
	if err == nil {
		for _, c := range allContainers {
			for _, n := range c.Names {
				if strings.HasPrefix(n, namePrefix) {
					_ = dockerCli.Client().ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
					break
				}
			}
		}
	}

	// Best-effort remove the default compose network for this project.
	_ = dockerCli.Client().NetworkRemove(ctx, projectName+"_default")
}

func startHasChangedImagesStack(ctx context.Context, t *testing.T, projectName string, composeContents string, serviceName string) (*testCompose.DockerCompose, error) {
	t.Helper()

	t.Chdir(t.TempDir())

	stack, err := testCompose.NewDockerComposeWith(
		testCompose.StackIdentifier(projectName),
		testCompose.WithStackReaders(strings.NewReader(composeContents)),
	)
	if err != nil {
		return nil, err
	}

	err = stack.
		WaitForService(serviceName, wait.ForListeningPort("80/tcp")).
		Up(ctx, testCompose.Wait(true))
	if err != nil {
		return nil, err
	}

	t.Cleanup(func() {
		_ = stack.Down(
			ctx,
			testCompose.RemoveOrphans(true),
			testCompose.RemoveVolumes(true),
			testCompose.RemoveImagesLocal,
		)
	})

	return stack, nil
}

func TestHasChangedImages(t *testing.T) {
	ctx := context.Background()

	projectName := "test-changed-images-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	serviceName := "web"

	composeLatest := `services:
  web:
    image: nginx:latest
    ports:
      - "80:80"
`

	dockerCli, err := CreateDockerCli(true, true)
	if err != nil {
		t.Fatalf("failed to create docker cli: %v", err)
	}

	cleanupComposeProjectContainers(ctx, t, dockerCli, projectName)

	_, err = startHasChangedImagesStack(ctx, t, projectName, composeLatest, serviceName)
	if err != nil {
		t.Fatalf("failed to start test stack: %v", err)
	}

	// Desired = latest, Deployed = latest => no drift
	tmpDirLatest := t.TempDir()
	composePathLatest := filepath.Join(tmpDirLatest, "compose.yaml")
	createComposeFile(t, composePathLatest, composeLatest)

	projectLatest, err := LoadCompose(ctx, tmpDirLatest, projectName, []string{composePathLatest}, []string{}, []string{}, map[string]string{})
	if err != nil {
		t.Fatalf("failed to load compose: %v", err)
	}

	changed, err := HasChangedImages(ctx, dockerCli, projectLatest)
	if err != nil {
		t.Fatalf("HasChangedImages returned error: %v", err)
	}

	if changed {
		t.Fatalf("expected no image changes, got changed=true")
	}

	// Desired = stable, Deployed = latest => drift
	// Desired = different image, Deployed = latest => drift
	composeDifferent := `services:
  web:
    image: httpd:latest
    ports:
      - "80:80"
`

	tmpDirStable := t.TempDir()
	composePathStable := filepath.Join(tmpDirStable, "compose.yaml")
	createComposeFile(t, composePathStable, composeDifferent)

	projectStable, err := LoadCompose(ctx, tmpDirStable, projectName, []string{composePathStable}, []string{}, []string{}, map[string]string{})
	if err != nil {
		t.Fatalf("failed to load compose (different): %v", err)
	}

	changed, err = HasChangedImages(ctx, dockerCli, projectStable)
	if err != nil {
		t.Fatalf("HasChangedImages returned error: %v", err)
	}

	if !changed {
		t.Fatalf("expected image changes, got changed=false")
	}
}
