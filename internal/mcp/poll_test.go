package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/common/types/duration"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/syncwindow"
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

func TestMCPTriggerPollReportsRunOutcomes(t *testing.T) {
	pauseErr := fmt.Errorf("web: %w", stages.ErrDeploymentPaused)

	for _, tc := range []struct {
		name    string
		outcome error
		wait    bool
		wantRun controlplane.RunStatus
	}{
		{"sync window", &stages.SyncWindowBlockedError{Stacks: []string{"web"}, Windows: []string{"freeze"}}, true, controlplane.RunStatusSkipped},
		{"retry suppressed", &stages.RetrySuppressedError{Stacks: []stages.RetrySuppressedStack{{Name: "web", Reason: "budget_exhausted"}}}, true, controlplane.RunStatusSkipped},
		{"paused", pauseErr, true, controlplane.RunStatusSkipped},
		{"paused async", pauseErr, false, controlplane.RunStatusSkipped},
		{"failed", errors.New("deployment failed"), true, controlplane.RunStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := logger.New(logger.LevelCritical)
			h := &Handler{log: log}
			h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				appConfig: &app.Config{},
				log:       log,
				pollRunner: func(ctx context.Context, cfg poll.Config, _ *app.Config, _ container.MountPoint,
					_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider, _ string,
				) error {
					if origin := controlplane.DeploymentOrigin(ctx); origin != syncwindow.OriginManual {
						t.Errorf("MCP poll origin = %q, want manual", origin)
					}

					if len(cfg.Deployments) != 1 || !cfg.Deployments[0].Pause {
						t.Errorf("manual MCP poll lost pause configuration: %#v", cfg)
					}

					return tc.outcome
				},
			})
			server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 4096, h)
			session := connectMCPTestClient(t, server)

			deployment := &deploy.Config{Name: "web", Pause: true}
			if err := json.Unmarshal([]byte(`{"environment":{},"external_secrets":{},"build":{"args":{}}}`), deployment); err != nil {
				t.Fatal(err)
			}

			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
				Name: "trigger_poll",
				Arguments: triggerPollInput{
					Configs: []triggerPollConfig{{
						SourceURL:   validPollSourceURL,
						Deployments: []*deploy.Config{deployment},
					}},
					Wait: new(tc.wait),
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			var output triggerPollOutput
			decodeMCPStructuredContent(t, result, &output)

			wantStatus := tc.wantRun
			if !tc.wait {
				wantStatus = controlplane.RunStatusAccepted
			}

			if output.JobID == "" || output.Status != string(wantStatus) || result.IsError != (tc.wantRun == controlplane.RunStatusFailed) {
				content, _ := json.Marshal(result.Content)
				t.Fatalf("unexpected run output: %#v, content %s, isError %t", output, content, result.IsError)
			}

			if tc.wait && tc.wantRun == controlplane.RunStatusSkipped {
				if len(result.Content) != 1 {
					t.Fatalf("skip must include one reason: %#v", result.Content)
				}

				if reason, ok := result.Content[0].(*sdkmcp.TextContent); !ok || reason.Text != tc.outcome.Error() {
					t.Fatalf("skip reason = %#v, want %q", result.Content, tc.outcome.Error())
				}
			}

			run := waitForDeploymentRunStatus(t, h.controlPlaneRuns, output.JobID, tc.wantRun)
			if tc.wantRun == controlplane.RunStatusSkipped && !strings.Contains(run.Message, tc.outcome.Error()) {
				t.Fatalf("run lost skip reason: %#v", run)
			}
		})
	}
}

func TestMCPTriggerPollPreservesManualOriginWithRetryPolicy(t *testing.T) {
	for _, wait := range []bool{true, false} {
		t.Run(strconv.FormatBool(wait), func(t *testing.T) {
			origin := make(chan syncwindow.Origin, 1)
			log := logger.New(logger.LevelCritical)
			h := &Handler{log: log}
			h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				appConfig: &app.Config{},
				log:       log,
				pollRunner: func(ctx context.Context, cfg poll.Config, _ *app.Config, _ container.MountPoint,
					_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider, _ string,
				) error {
					if len(cfg.Deployments) != 1 || cfg.Deployments[0].Retry.Limit == nil ||
						*cfg.Deployments[0].Retry.Limit != 0 {
						t.Errorf("manual poll lost retry configuration: %#v", cfg)
					}

					origin <- controlplane.DeploymentOrigin(ctx)

					return nil
				},
			})
			server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 4096, h)
			session := connectMCPTestClient(t, server)

			deployment := &deploy.Config{
				Name: "web",
				Retry: deploy.RetryConfig{
					Limit: new(0),
					Delay: duration.Duration(time.Hour),
				},
			}
			if err := json.Unmarshal([]byte(`{"environment":{},"external_secrets":{},"build":{"args":{}}}`), deployment); err != nil {
				t.Fatal(err)
			}

			result := callMCPTool(t, session, "trigger_poll", triggerPollInput{
				Configs: []triggerPollConfig{{
					SourceURL:   validPollSourceURL,
					Deployments: []*deploy.Config{deployment},
				}},
				Wait: new(wait),
			})

			var output triggerPollOutput
			decodeMCPStructuredContent(t, result, &output)

			if output.JobID == "" || result.IsError {
				t.Fatalf("manual MCP trigger failed: %#v, %#v", output, result)
			}

			select {
			case got := <-origin:
				if got != syncwindow.OriginManual {
					t.Fatalf("origin = %s, want manual", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("manual MCP trigger did not execute")
			}

			waitForDeploymentRunStatus(t, h.controlPlaneRuns, output.JobID, controlplane.RunStatusSucceeded)
		})
	}
}
