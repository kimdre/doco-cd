package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"
	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/git"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/logger"
)

func TestPollHandlerAllowsConcurrentRunsForSameRepository(t *testing.T) {
	log := logger.New(logger.LevelCritical)
	started := make(chan string, 2)
	release := make(chan struct{})

	h := handlerData{
		log: log,
		runPoll: func(_ context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *slog.Logger, metadata notification.Metadata, _ *secretprovider.SecretProvider,
		) error {
			started <- metadata.JobID

			<-release

			return nil
		},
	}

	jobConfig := poll.Config{
		SourceUrl: "https://github.com/kimdre/doco-cd_tests.git",
		Reference: "main",
		Interval:  poll.MinPollInterval,
		RunOnce:   true,
	}

	ctx := t.Context()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()

		h.PollHandler(ctx, &poll.Job{Config: jobConfig})
	}()

	go func() {
		defer wg.Done()

		h.PollHandler(ctx, &poll.Job{Config: jobConfig})
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(500 * time.Millisecond):
			close(release)
			t.Fatal("expected both poll handlers to start their runs without serializing on the repository")
		}
	}

	close(release)

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poll handlers did not exit after their run_once executions completed")
	}
}

func TestRunPoll(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	log := logger.New(logger.LevelCritical)
	ctx := context.Background()

	pollConfig := poll.Config{
		SourceUrl:    "https://github.com/kimdre/doco-cd_tests.git",
		Reference:    "main",
		Interval:     10 * time.Second,
		CustomTarget: "",
	}

	stackName := test.ConvertTestName(t.Name())

	if swarm.GetModeEnabled() {
		pollConfig.Reference = git.SwarmModeBranch

		t.Log("Testing in Swarm mode, using 'swarm-mode' reference")
	}

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.GitCommitStatus = false

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

	dockerCli, err := docker.CreateDockerCli(appConfig.DockerQuietDeploy)
	if err != nil {
		t.Fatalf("Failed to create docker client: %v", err)
	}

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
		if swarm.GetModeEnabled() {
			err = docker.RemoveSwarmStack(ctx, dockerCli, stackName)
		} else {
			err = service.Down(ctx, stackName, downOpts)
		}

		if err != nil {
			t.Fatal(err)
		}
	})

	metadata := notification.Metadata{
		Repository: git.GetRepoName(pollConfig.SourceUrl),
		Stack:      stackName,
		Revision:   notification.GetRevision(pollConfig.Reference, ""),
	}

	// Run initial poll
	if err := RunPoll(ctx, pollConfig, appConfig, dataMountPoint, dockerCli, log.With(), metadata, &secretProvider); err != nil {
		t.Fatalf("Initial poll deployment failed: %v", err)
	}

	pollConfig.Reference = "destroy"

	// Run the second poll to destroy
	if err := RunPoll(ctx, pollConfig, appConfig, dataMountPoint, dockerCli, log.With(), metadata, &secretProvider); err != nil {
		t.Fatalf("Second poll deployment failed: %v", err)
	}
}
