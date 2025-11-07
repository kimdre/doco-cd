package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"

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
		Repository: getRepoName(string(pollConfig.CloneUrl)),
		Stack:      name,
		Revision:   notification.GetRevision(pollConfig.Reference, ""),
	}

	// Run initial poll
	_, err = RunPoll(ctx, pollConfig, appConfig, dataMountPoint, dockerCli, dockerClient, log.With(), metadata, &secretProvider)
	if err != nil {
		t.Fatalf("Initial poll failed: %v", err)
	}

	pollConfig.Reference = "destroy"

	// Run the second poll to destroy
	_, err = RunPoll(ctx, pollConfig, appConfig, dataMountPoint, dockerCli, dockerClient, log.With(), metadata, &secretProvider)
	if err != nil {
		t.Fatalf("Second poll failed: %v", err)
	}
}

func TestResolveDeployConfigs(t *testing.T) {
	t.Run("returns inline deployments when provided", func(t *testing.T) {
		testLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
		pollConfig := config.PollConfig{
			CloneUrl:  "https://example.com/repo.git",
			Reference: "main",
			Deployments: []*config.DeployConfig{
				{
					Name:         "inline-app",
					ComposeFiles: []string{"compose.yaml"},
					RepositoryUrl: config.HttpUrl("https://example.com/runtime.git"),
				},
			},
		}

		if err := pollConfig.Validate(); err != nil {
			t.Fatalf("expected inline poll config to validate: %v", err)
		}

		configs, err := resolveDeployConfigs(pollConfig, t.TempDir(), "ignored", testLogger)
		if err != nil {
			t.Fatalf("expected inline deployments to be returned: %v", err)
		}

		if len(configs) != 1 {
			t.Fatalf("expected 1 deployment, got %d", len(configs))
		}

		if configs[0].Name != "inline-app" {
			t.Fatalf("expected deployment name to be inline-app, got %s", configs[0].Name)
		}

		if configs[0].Reference != pollConfig.Reference {
			t.Fatalf("expected deployment reference to match poll reference, got %s", configs[0].Reference)
		}

		if configs[0].RepositoryUrl != "" {
			t.Fatalf("expected repository_url to be cleared for inline deployments, got %s", configs[0].RepositoryUrl)
		}
	})

	t.Run("falls back to repository configuration when inline is absent", func(t *testing.T) {
		testLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
		tmpDir := t.TempDir()

		pollConfig := config.PollConfig{
			CloneUrl:  "https://example.com/repo.git",
			Reference: "main",
		}

		if err := pollConfig.Validate(); err != nil {
			t.Fatalf("expected poll config without inline deployments to validate: %v", err)
		}

		configs, err := resolveDeployConfigs(pollConfig, tmpDir, "default", testLogger)
		if err != nil {
			t.Fatalf("expected default deployment to be returned: %v", err)
		}

		if len(configs) != 1 {
			t.Fatalf("expected 1 deployment, got %d", len(configs))
		}

		if configs[0].Name != "default" {
			t.Fatalf("expected default deployment name to be default, got %s", configs[0].Name)
		}

		if configs[0].Reference != pollConfig.Reference {
			t.Fatalf("expected default deployment reference to match poll reference, got %s", configs[0].Reference)
		}
	})
}
