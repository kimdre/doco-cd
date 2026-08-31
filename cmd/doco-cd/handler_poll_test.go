package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
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

func TestPollConfigLogValueRedactsSensitiveFields(t *testing.T) {
	t.Parallel()

	const secret = "sentinel-secret"

	var output bytes.Buffer

	log := slog.New(slog.NewTextHandler(&output, nil))
	config := poll.Config{
		SourceUrl: "https://user:" + secret + "@example.com/repository.git",
		Reference: "main",
		Deployments: []*deploy.Config{{
			Name:          "production",
			RepositoryUrl: "https://user:" + secret + "@example.com/deployment.git",
			Environment:   map[string]string{"TOKEN": secret},
			ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
				"PASSWORD": {LegacyRef: secret},
			},
		}},
	}

	log.Info("polling", slog.Attr{Key: "config", Value: pollConfigLogValue(config)})

	logged := output.String()

	if strings.Contains(logged, secret) {
		t.Fatalf("poll config log leaked secret: %q", logged)
	}

	if !strings.Contains(logged, "production") || !strings.Contains(logged, "reference:main") {
		t.Fatalf("poll config log lost safe identifiers: %q", logged)
	}
}

func TestPollHandlerAllowsConcurrentRunsForSameRepository(t *testing.T) {
	log := logger.New(logger.LevelCritical)
	started := make(chan notification.Metadata, 2)
	release := make(chan struct{})

	h := orchestrationHandler{
		log: log,

		controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			log: log,
			pollRunner: func(_ context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
				_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, metadata notification.Metadata, _ secretprovider.SecretProvider,
				_ string,
			) error {
				started <- metadata

				<-release

				return nil
			},
		}),
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

func TestPollHandlerRunOnceDoesNotStartLocalWatcher(t *testing.T) {
	var output bytes.Buffer

	log := &logger.Logger{
		Logger: slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Level:  slog.LevelDebug,
	}
	srcPath := createLocalPollTestRepository(t)

	h := orchestrationHandler{
		log: log,

		controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			log: log,
			pollRunner: func(_ context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
				_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider,
				_ string,
			) error {
				return nil
			},
		}),
	}

	pollJob := &poll.Job{Config: poll.Config{
		Source:    config.SourceTypeGit,
		SourceUrl: "file://" + srcPath,
		Reference: "main",
		RunOnce:   true,
		Watch:     true,
	}}
	h.PollHandler(t.Context(), pollJob)

	logged := output.String()
	if strings.Contains(logged, "watching local repository for changes") {
		t.Fatal("run_once poll started a local repository watcher")
	}

	if strings.Contains(logged, "falling back to safety-net poll interval") {
		t.Fatal("run_once poll enabled watcher fallback polling")
	}

	if pollJob.NextRun != 0 {
		t.Fatalf("run_once poll next run = %d, want 0", pollJob.NextRun)
	}
}

func TestPollHandlerTracksCustomTarget(t *testing.T) {
	log := logger.New(logger.LevelCritical)
	h := orchestrationHandler{
		log: log,

		controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			log: log,
			pollRunner: func(_ context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
				_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider,
				_ string,
			) error {
				return nil
			},
		}),
	}

	h.PollHandler(t.Context(), &poll.Job{Config: poll.Config{
		SourceUrl:    "https://github.com/kimdre/doco-cd_tests.git",
		Reference:    "main",
		CustomTarget: "prod-vm",
		RunOnce:      true,
	}})

	runs := h.controlPlaneRuns.List(1, string(controlplane.RunTriggerPoll), "")
	if len(runs) != 1 {
		t.Fatalf("expected one tracked poll run, got %d", len(runs))
	}

	if runs[0].Target != "prod-vm" {
		t.Fatalf("tracked target = %q, want %q", runs[0].Target, "prod-vm")
	}
}

// TestWatcherClosedFallback deterministically covers both branches of the
// watcher-closed handling: shutdown must stop the handler without the
// fallback warning, while an unexpected watcher death must log the warning
// and select the configured or safety-net interval.
func TestWatcherClosedFallback(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name         string
		cancelled    bool
		interval     time.Duration
		wantStop     bool
		wantInterval time.Duration
		wantWarning  bool
	}{
		{name: "shutdown stops without fallback", cancelled: true, interval: 0, wantStop: true},
		{name: "unexpected close falls back to safety net", interval: 0, wantInterval: pollWatcherlessFallbackInterval, wantWarning: true},
		{name: "unexpected close keeps configured interval", interval: 5 * time.Minute, wantInterval: 5 * time.Minute, wantWarning: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			if testCase.cancelled {
				cancelledCtx, cancel := context.WithCancel(ctx)
				cancel()

				ctx = cancelledCtx
			}

			var output bytes.Buffer

			log := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))

			gotInterval, gotStop := watcherClosedFallback(ctx, log, testCase.interval)
			if gotStop != testCase.wantStop {
				t.Fatalf("stop = %t, want %t", gotStop, testCase.wantStop)
			}

			if !gotStop && gotInterval != testCase.wantInterval {
				t.Fatalf("interval = %s, want %s", gotInterval, testCase.wantInterval)
			}

			warned := strings.Contains(output.String(), "local repository watcher closed, continuing with interval polling")
			if warned != testCase.wantWarning {
				t.Fatalf("warning logged = %t, want %t: %q", warned, testCase.wantWarning, output.String())
			}
		})
	}
}

func TestPollHandlerShutdownDoesNotEnableWatcherFallback(t *testing.T) {
	for range 20 {
		var output bytes.Buffer

		log := &logger.Logger{
			Logger: slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})),
			Level:  slog.LevelDebug,
		}
		srcPath := createLocalPollTestRepository(t)
		started := make(chan struct{})
		release := make(chan struct{})
		h := orchestrationHandler{
			log: log,

			controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				log: log,
				pollRunner: func(_ context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
					_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider,
					_ string,
				) error {
					close(started)
					<-release

					return nil
				},
			}),
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})

		go func() {
			defer close(done)

			h.PollHandler(ctx, &poll.Job{Config: poll.Config{
				Source:    config.SourceTypeGit,
				SourceUrl: "file://" + srcPath,
				Reference: "main",
				Watch:     true,
			}})
		}()

		<-started
		cancel()
		close(release)
		<-done

		if strings.Contains(output.String(), "local repository watcher closed, continuing with interval polling") {
			t.Fatal("application shutdown enabled watcher fallback polling")
		}
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
	if err := RunPoll(ctx, pollConfig, appConfig, dataMountPoint, dockerCli, nil, log.With(), metadata, secretProvider, pollTriggerDefault); err != nil {
		t.Fatalf("Initial poll deployment failed: %v", err)
	}

	pollConfig.Reference = "destroy"

	// Run the second poll to destroy
	if err := RunPoll(ctx, pollConfig, appConfig, dataMountPoint, dockerCli, nil, log.With(), metadata, secretProvider, pollTriggerDefault); err != nil {
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

	h := orchestrationHandler{
		log: log,

		controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			log: log,
			pollRunner: func(_ context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
				_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider,
				_ string,
			) error {
				runCount.Add(1)

				return nil
			},
		}),
	}

	jobConfig := poll.Config{
		Source:    config.SourceTypeGit,
		SourceUrl: "file:///nonexistent/path/that/does/not/exist",
		Reference: "main",
		Interval:  0, // watcher-only mode; watcher creation will fail for this path
		Watch:     true,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	h.PollHandler(ctx, &poll.Job{Config: jobConfig})

	// With the fix, the fallback interval is a 24h safety net, so within
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

	h := orchestrationHandler{
		log: log,

		controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			log: log,
			pollRunner: func(_ context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
				_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider,
				triggerReason string,
			) error {
				reasons <- triggerReason

				return nil
			},
		}),
	}

	jobConfig := poll.Config{
		Source:    config.SourceTypeGit,
		SourceUrl: "file://" + srcPath,
		Reference: "main",
		Interval:  0, // watcher-only mode
		Watch:     true,
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

func createLocalPollTestRepository(t *testing.T) string {
	t.Helper()

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

	if err := os.WriteFile(filepath.Join(srcPath, "initial.txt"), []byte("initial\n"), 0o600); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	if _, err := wt.Add("initial.txt"); err != nil {
		t.Fatalf("add initial file: %v", err)
	}

	if _, err := wt.Commit("initial commit", &gogit.CommitOptions{Author: &object.Signature{
		Name:  "test",
		Email: "test@example.com",
		When:  time.Now(),
	}}); err != nil {
		t.Fatalf("commit initial file: %v", err)
	}

	return srcPath
}

// TestPollHandlerWatchDisabledFallsBackTo24h verifies that setting
// Watch: false on a local git poll config skips starting the filesystem
// watcher entirely, even for an otherwise valid local repository, and falls
// back to the 24h safety-net poll interval when no Interval is configured.
func TestPollHandlerWatchDisabledFallsBackTo24h(t *testing.T) {
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

	if err := os.WriteFile(filepath.Join(srcPath, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	if _, err := wt.Add("a.txt"); err != nil {
		t.Fatalf("add a.txt: %v", err)
	}

	if _, err := wt.Commit("initial commit", &gogit.CommitOptions{Author: sig}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var runCount atomic.Int32

	h := orchestrationHandler{
		log: log,

		controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			log: log,
			pollRunner: func(_ context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
				_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider,
				_ string,
			) error {
				runCount.Add(1)

				return nil
			},
		}),
	}

	jobConfig := poll.Config{
		Source:    config.SourceTypeGit,
		SourceUrl: "file://" + srcPath,
		Reference: "main",
		Interval:  0, // no interval, and watcher disabled below
		Watch:     false,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	h.PollHandler(ctx, &poll.Job{Config: jobConfig})

	// With Watch disabled and no Interval, the handler must fall back to a
	// 24h safety-net interval instead of busy-looping, so within 500ms only
	// the initial startup run should have happened.
	if got := runCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 run (startup only) with watch disabled and no interval, got %d", got)
	}
}

// TestPollHandlerWatcherOnlyModeHasNoPeriodicFallback verifies that when Watch
// is enabled and the watcher starts successfully with Interval: 0, no periodic
// timer is armed at all (the 24h fallback was removed) - only the initial
// startup run happens until a new commit triggers the watcher.
func TestPollHandlerWatcherOnlyModeHasNoPeriodicFallback(t *testing.T) {
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

	if err := os.WriteFile(filepath.Join(srcPath, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	if _, err := wt.Add("a.txt"); err != nil {
		t.Fatalf("add a.txt: %v", err)
	}

	if _, err := wt.Commit("initial commit", &gogit.CommitOptions{Author: sig}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var runCount atomic.Int32

	h := orchestrationHandler{
		log: log,

		controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			log: log,
			pollRunner: func(_ context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
				_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider,
				_ string,
			) error {
				runCount.Add(1)

				return nil
			},
		}),
	}

	jobConfig := poll.Config{
		Source:    config.SourceTypeGit,
		SourceUrl: "file://" + srcPath,
		Reference: "main",
		Interval:  0, // watcher-only mode, watcher should start successfully here
		Watch:     true,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()

	h.PollHandler(ctx, &poll.Job{Config: jobConfig})

	if got := runCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 run (startup only, no periodic fallback) with a working watcher and no interval, got %d", got)
	}
}
