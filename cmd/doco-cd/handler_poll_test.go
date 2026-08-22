package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/config"
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
	started := make(chan notification.Metadata, 2)
	release := make(chan struct{})

	h := handlerData{
		log: log,
		runPoll: func(_ context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *slog.Logger, metadata notification.Metadata, _ *secretprovider.SecretProvider,
			_ string,
		) error {
			started <- metadata

			<-release

			return nil
		},
	}

	jobConfig := poll.Config{
		SourceUrl:    "https://github.com/kimdre/doco-cd_tests.git",
		Reference:    "main",
		Interval:     poll.MinPollInterval,
		CustomTarget: "prod-vm",
		RunOnce:      true,
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
		case metadata := <-started:
			if metadata.JobID == "" {
				close(release)
				t.Fatal("expected poll metadata to include a job id")
			}

			if metadata.Target != "prod-vm" {
				close(release)
				t.Fatalf("expected poll metadata target prod-vm, got %q", metadata.Target)
			}
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
	if err := RunPoll(ctx, pollConfig, appConfig, dataMountPoint, dockerCli, log.With(), metadata, &secretProvider, pollTriggerDefault); err != nil {
		t.Fatalf("Initial poll deployment failed: %v", err)
	}

	pollConfig.Reference = "destroy"

	// Run the second poll to destroy
	if err := RunPoll(ctx, pollConfig, appConfig, dataMountPoint, dockerCli, log.With(), metadata, &secretProvider, pollTriggerDefault); err != nil {
		t.Fatalf("Second poll deployment failed: %v", err)
	}
}

// TestPollHandlerFallsBackWhenWatcherFailsWithZeroInterval ensures that when a
// local git source is configured with interval=0 (watcher-only mode) but the
// filesystem watcher fails to start (e.g. an invalid/missing repository path),
// PollHandler falls back to a safe minimum interval instead of busy-looping on
// time.After(0).
func TestPollHandlerFallsBackWhenWatcherFailsWithZeroInterval(t *testing.T) {
	log := logger.New(logger.LevelCritical)

	var runCount atomic.Int32

	h := handlerData{
		log: log,
		runPoll: func(_ context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *slog.Logger, _ notification.Metadata, _ *secretprovider.SecretProvider,
			_ string,
		) error {
			runCount.Add(1)

			return nil
		},
	}

	jobConfig := poll.Config{
		Source:    config.SourceTypeGit,
		SourceUrl: "file:///nonexistent/path/that/does/not/exist",
		Reference: "main",
		Interval:  0, // watcher-only mode; watcher creation will fail for this path
	}

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	h.PollHandler(ctx, &poll.Job{Config: jobConfig})

	// With the fix, the fallback interval is poll.MinPollInterval (10s), so within
	// 500ms only the initial startup run should have happened. Without the fix,
	// time.After(0) would fire continuously and runCount would be much higher.
	if got := runCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 run (startup) within 500ms fallback window, got %d (possible busy loop)", got)
	}
}

// TestPollHandlerReportsWatchTriggerReason verifies that a poll run triggered by
// the local repository filesystem watcher reports a distinct trigger reason
// ("poll-watch") from the initial startup run ("poll"), so log lines can tell
// interval/startup-driven polls apart from watcher-driven ones.
func TestPollHandlerReportsWatchTriggerReason(t *testing.T) {
	log := logger.New(logger.LevelCritical)

	srcPath := t.TempDir()

	repo, err := gogit.PlainInit(srcPath, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}

	writeAndCommit := func(name, content, msg string) {
		if err := os.WriteFile(filepath.Join(srcPath, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}

		if _, err := wt.Add(name); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}

		if _, err := wt.Commit(msg, &gogit.CommitOptions{Author: sig}); err != nil {
			t.Fatalf("commit %s: %v", msg, err)
		}
	}

	writeAndCommit("a.txt", "a\n", "initial commit")

	reasons := make(chan string, 10)

	h := handlerData{
		log: log,
		runPoll: func(_ context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *slog.Logger, _ notification.Metadata, _ *secretprovider.SecretProvider,
			triggerReason string,
		) error {
			reasons <- triggerReason

			return nil
		},
	}

	jobConfig := poll.Config{
		Source:    config.SourceTypeGit,
		SourceUrl: "file://" + srcPath,
		Reference: "main",
		Interval:  0, // watcher-only mode
	}

	ctx := t.Context()

	go h.PollHandler(ctx, &poll.Job{Config: jobConfig})

	select {
	case r := <-reasons:
		if r != pollTriggerDefault {
			t.Fatalf("expected startup run to report trigger reason %q, got %q", pollTriggerDefault, r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for startup poll run")
	}

	writeAndCommit("b.txt", "b\n", "second commit")

	select {
	case r := <-reasons:
		if r != pollTriggerWatch {
			t.Fatalf("expected watch-triggered run to report trigger reason %q, got %q", pollTriggerWatch, r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for watch-triggered poll run")
	}
}
