package main

import (
	"context"
	"testing"

	"github.com/kimdre/doco-cd/internal/git"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/logger"
)

func TestRunPoll(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	log := logger.New(12)
	ctx := context.Background()

	pollConfig := config.PollConfig{
		CloneUrl:     "https://github.com/kimdre/doco-cd_tests.git",
		Reference:    "main",
		Interval:     10,
		CustomTarget: "",
		Private:      true,
	}

	if docker.SwarmModeEnabled {
		pollConfig.Reference = git.SwarmModeBranch

		t.Log("Testing in Swarm mode, using 'swarm-mode' reference")
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

	service := compose.NewComposeService(dockerCli)

	downOpts := api.DownOptions{
		RemoveOrphans: true,
		Images:        "all",
		Volumes:       true,
	}

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

	t.Cleanup(func() {
		t.Log("Remove test container")

		err = service.Down(ctx, "test-deploy", downOpts)
		if err != nil {
			t.Fatal(err)
		}
	})

	// Run initial poll
	err = RunPoll(ctx, pollConfig, appConfig, dataMountPoint, dockerCli, dockerClient, log.With())
	if err != nil {
		t.Fatalf("Initial poll failed: %v", err)
	}

	pollConfig.Reference = "destroy"

	// Run the second poll to destroy
	err = RunPoll(ctx, pollConfig, appConfig, dataMountPoint, dockerCli, dockerClient, log.With())
	if err != nil {
		t.Fatalf("Second poll failed: %v", err)
	}
}
