package main

import (
	"context"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
)

func TestRunPoll(t *testing.T) {
	log := logger.New(12)
	ctx := context.Background()

	pollConfig := config.PollConfig{
		CloneUrl:     "https://github.com/kimdre/doco-cd.git",
		Reference:    "main",
		Interval:     10,
		CustomTarget: "",
		Private:      false,
	}

	appConfig, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	dockerCli, err := docker.CreateDockerCli(appConfig.DockerQuietDeploy, !appConfig.SkipTLSVerification)
	if err != nil {
		t.Fatalf("Failed to create docker client: %v", err)
	}

	dockerClient, _ := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)

	tmpDir := t.TempDir()
	dataMountPoint := container.MountPoint{
		Type:        "bind",
		Source:      tmpDir,
		Destination: tmpDir,
		Mode:        "rw",
	}

	t.Cleanup(func() {
		err = dockerCli.Client().Close()
		if err != nil {
			return
		}
	})

	// Run initial poll
	err = RunPoll(ctx, pollConfig, appConfig, dataMountPoint, dockerCli, dockerClient, log.With())
	if err != nil {
		t.Fatalf("RunPoll failed: %v", err)
	}

	// Run the second poll
	err = RunPoll(ctx, pollConfig, appConfig, dataMountPoint, dockerCli, dockerClient, log.With())
	if err != nil {
		t.Fatalf("RunPoll failed: %v", err)
	}

	// Check if the deployed test container is running
	testContainerID, err := docker.GetContainerID(dockerCli.Client(), "test")
	if err != nil {
		t.Fatal(err)
	}

	service := compose.NewComposeService(dockerCli)

	downOpts := api.DownOptions{
		RemoveOrphans: true,
		Images:        "all",
		Volumes:       true,
	}

	t.Cleanup(func() {
		t.Log("Remove test container")

		err = service.Down(ctx, "test-deploy", downOpts)
		if err != nil {
			t.Fatal(err)
		}
	})

	testContainer, err := dockerCli.Client().ContainerInspect(ctx, testContainerID)
	if err != nil {
		t.Fatal(err)
	}

	if testContainer.State.Running != true {
		t.Errorf("Test container is not running: %v", testContainer.State)
	}
}
