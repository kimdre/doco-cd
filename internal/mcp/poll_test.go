package mcp

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

const validPollSourceURL = "https://github.com/kimdre/doco-cd_tests.git"

func TestMCPTriggerPollValidationAndDefaultWait(t *testing.T) {
	runs := 0
	log := logger.New(logger.LevelCritical)
	appConfig := &app.Config{}
	h := &Handler{log: log}
	h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig: appConfig,
		log:       log,
		pollRunner: func(_ context.Context, cfg poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider, _ string,
		) error {
			runs++

			if !cfg.RunOnce || cfg.Interval != 0 || cfg.Reference != git.MainBranch {
				t.Errorf("poll defaults = %#v", cfg)
			}

			return nil
		},
	})
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	assertMCPToolError(t, session, "trigger_poll", map[string]any{}, "configs")
	assertMCPToolError(t, session, "trigger_poll", map[string]any{"configs": []any{}}, "no poll configuration provided in request body")
	assertMCPToolError(t, session, "trigger_poll", map[string]any{"configs": []any{map[string]any{}}}, "url")

	if got := h.controlPlaneRuns.List(10, string(controlplane.RunTriggerPoll), ""); len(got) != 0 {
		t.Fatalf("invalid MCP poll requests were tracked: %#v", got)
	}

	result := callMCPTool(t, session, "trigger_poll", map[string]any{
		"configs": []any{map[string]any{"url": validPollSourceURL}},
	})

	var output triggerPollOutput
	decodeMCPStructuredContent(t, result, &output)

	if output.JobID == "" || output.Status != string(controlplane.RunStatusSucceeded) || runs != 1 {
		t.Fatalf("unexpected trigger output: %#v, runs %d", output, runs)
	}
}

func TestMCPTriggerPollAsyncJobIDResolves(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	log := logger.New(logger.LevelCritical)
	appConfig := &app.Config{}
	h := &Handler{log: log}
	h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig: appConfig,
		log:       log,
		pollRunner: func(ctx context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider, _ string,
		) error {
			close(started)
			<-release

			if ctx.Err() != nil {
				t.Errorf("async poll context was cancelled: %v", ctx.Err())
			}

			return nil
		},
	})
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)
	result := callMCPTool(t, session, "trigger_poll", map[string]any{
		"configs": []any{map[string]any{"url": validPollSourceURL}},
		"wait":    false,
	})

	var output triggerPollOutput
	decodeMCPStructuredContent(t, result, &output)

	if output.JobID == "" || output.Status != string(controlplane.RunStatusAccepted) {
		t.Fatalf("unexpected async output: %#v", output)
	}

	getResult := callMCPTool(t, session, "get_deployment_run", map[string]any{"job_id": output.JobID})

	var getOutput getDeploymentRunOutput
	decodeMCPStructuredContent(t, getResult, &getOutput)

	if getOutput.Run.JobID != output.JobID || getOutput.Run.Trigger != controlplane.RunTriggerPoll {
		t.Fatalf("async job ID did not resolve: %#v", getOutput.Run)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("async poll did not start")
	}

	close(release)
	waitForDeploymentRunStatus(t, h.controlPlaneRuns, output.JobID, controlplane.RunStatusSucceeded)
}
