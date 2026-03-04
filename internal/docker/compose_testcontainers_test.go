// TODO: Re-enable when testcontainers-go/modules/compose migrates to docker/compose/v5
//go:build ignore

package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	testCompose "github.com/testcontainers/testcontainers-go/modules/compose"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/test"
)

func startTestContainer(ctx context.Context, t *testing.T) (*testCompose.DockerCompose, error) {
	stackName := test.ConvertTestName(t.Name())

	composeYAML := generateComposeContents()

	stack, err := testCompose.NewDockerComposeWith(
		testCompose.StackIdentifier(stackName),
		testCompose.WithStackReaders(strings.NewReader(composeYAML)),
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

	err = RestartProject(ctx, dockerCli, test.ConvertTestName(t.Name()), timeout)
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

	err = StopProject(ctx, dockerCli, test.ConvertTestName(t.Name()), timeout)
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
