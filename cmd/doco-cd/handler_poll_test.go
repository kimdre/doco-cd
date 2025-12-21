package main

import (
	"context"
	"testing"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"

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
	}

	const name = "test-deploy"

	if swarm.ModeEnabled {
		pollConfig.Reference = git.SwarmModeBranch

		t.Log("Testing in Swarm mode, using 'swarm-mode' reference")
	}

	appConfig, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	secretProvider, err := secretprovider.Initialize(ctx, appConfig.SecretProvider, "v0.0.0-test")
	if err != nil {
		t.Fatalf("failed to initialize secret provider: %s", err.Error())

		return
	}

	if secretProvider != nil {
		t.Cleanup(func() {
			secretProvider.Close()
		})
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

		if swarm.ModeEnabled {
			err = docker.RemoveSwarmStack(ctx, dockerCli, name)
		} else {
			err = service.Down(ctx, name, downOpts)
		}

		if err != nil {
			t.Fatal(err)
		}
	})

	metadata := notification.Metadata{
		Repository: stages.GetRepoName(string(pollConfig.CloneUrl)),
		Stack:      name,
		Revision:   notification.GetRevision(pollConfig.Reference, ""),
	}

	// Run initial poll
	results := RunPoll(ctx, pollConfig, appConfig, dataMountPoint, dockerCli, dockerClient, log.With(), metadata, &secretProvider)

	for _, result := range results {
		if result.Err != nil {
			t.Fatalf("Initial poll deployment failed: %v", result.Err)
		}
	}

	pollConfig.Reference = "destroy"

	// Run the second poll to destroy
	results = RunPoll(ctx, pollConfig, appConfig, dataMountPoint, dockerCli, dockerClient, log.With(), metadata, &secretProvider)
	for _, result := range results {
		if result.Err != nil {
			t.Fatalf("Second poll deployment failed: %v", result.Err)
		}
	}
}
