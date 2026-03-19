package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"
	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/git"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/logger"
)

func TestRunPoll(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	log := logger.New(logger.LevelCritical)
	ctx := context.Background()

	pollConfig := config.PollConfig{
		CloneUrl:     "https://github.com/kimdre/doco-cd_tests.git",
		Reference:    "main",
		Interval:     10,
		CustomTarget: "",
	}

	stackName := test.ConvertTestName(t.Name())

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
		if errors.Is(err, bitwardensecretsmanager.ErrNotSupported) {
			t.Skip(err.Error())
		}

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

	dockerClient, _ := client.New(
		client.FromEnv,
	)

	service, err := compose.NewComposeService(dockerCli)
	if err != nil {
		t.Fatalf("failed to create compose service: %v", err)
	}

	downOpts := api.DownOptions{
		RemoveOrphans: true,
		Images:        "local",
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
		if swarm.ModeEnabled {
			err = docker.RemoveSwarmStack(ctx, dockerCli, stackName)
		} else {
			err = service.Down(ctx, stackName, downOpts)
		}

		if err != nil {
			t.Fatal(err)
		}
	})

	metadata := notification.Metadata{
		Repository: git.GetRepoName(string(pollConfig.CloneUrl)),
		Stack:      stackName,
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
