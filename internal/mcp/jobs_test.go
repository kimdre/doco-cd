package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/test"
)

func TestMCPTriggerScheduledJobValidation(t *testing.T) {
	h := &Handler{log: logger.New(logger.LevelCritical)}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	assertMCPToolError(t, session, "trigger_scheduled_job", map[string]any{}, "job_name")
	assertMCPToolError(t, session, "trigger_scheduled_job", map[string]any{"job_name": "  "}, "missing job name")
}

func TestMCPTriggerScheduledJobDefaultsToWait(t *testing.T) {
	triggered := make(chan context.Context, 1)

	dockerCli, err := command.NewDockerCli()
	if err != nil {
		t.Fatal(err)
	}

	h := &Handler{
		dockerCli: dockerCli,
		log:       logger.New(logger.LevelCritical),
	}
	h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		log: h.log,
		scheduledJobs: testScheduledJobOperations{triggerNow: func(
			ctx context.Context,
			_ string,
			jobName string,
			stack string,
			_ secretprovider.SecretProvider,
		) (string, error) {
			if jobName != "backup" || stack != "prod" {
				t.Errorf("trigger arguments = %q, %q", jobName, stack)
			}

			triggered <- ctx

			return "scheduled-run-id", nil
		}},
	})
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	result := callMCPTool(t, connectMCPTestClient(t, server), "trigger_scheduled_job", map[string]any{"job_name": " backup ", "stack": " prod "})

	var output triggerScheduledJobOutput
	decodeMCPStructuredContent(t, result, &output)

	if output.JobID == "" || output.Status != string(controlplane.RunStatusSucceeded) {
		t.Fatalf("unexpected trigger output: %#v", output)
	}

	select {
	case <-triggered:
	case <-time.After(time.Second):
		t.Fatal("sync trigger was not called")
	}
}

func TestMCPTriggerScheduledJobAsyncJobIDResolves(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	dockerCli, err := command.NewDockerCli()
	if err != nil {
		t.Fatal(err)
	}

	h := &Handler{
		dockerCli: dockerCli,
		log:       logger.New(logger.LevelCritical),
	}
	h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		log: h.log,
		scheduledJobs: testScheduledJobOperations{triggerNow: func(
			ctx context.Context,
			_ string,
			_ string,
			_ string,
			_ secretprovider.SecretProvider,
		) (string, error) {
			close(started)
			<-release

			if ctx.Err() != nil {
				t.Errorf("async trigger context was cancelled: %v", ctx.Err())
			}

			return "scheduled-run-id", nil
		}},
	})
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)
	result := callMCPTool(t, session, "trigger_scheduled_job", map[string]any{"job_name": "backup", "wait": false})

	var output triggerScheduledJobOutput
	decodeMCPStructuredContent(t, result, &output)

	if output.JobID == "" || output.Status != string(controlplane.RunStatusAccepted) {
		t.Fatalf("unexpected async output: %#v", output)
	}

	getResult := callMCPTool(t, session, "get_deployment_run", map[string]any{"job_id": output.JobID})

	var getOutput getDeploymentRunOutput
	decodeMCPStructuredContent(t, getResult, &getOutput)

	if getOutput.Run.JobID != output.JobID || getOutput.Run.Trigger != controlplane.RunTriggerScheduledJob {
		t.Fatalf("async job ID did not resolve: %#v", getOutput.Run)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("async trigger did not start")
	}

	close(release)
	waitForDeploymentRunStatus(t, h.controlPlaneRuns, output.JobID, controlplane.RunStatusSucceeded)
}

// TestMCPTriggerScheduledJobErrorKeepsJobID pins the trigger error contract:
// the tool reports IsError with the failure text while the structured output
// still carries the job ID and failed status, and the job ID resolves through
// get_deployment_run.
func TestMCPTriggerScheduledJobErrorKeepsJobID(t *testing.T) {
	dockerCli, err := command.NewDockerCli()
	if err != nil {
		t.Fatal(err)
	}

	h := &Handler{
		dockerCli: dockerCli,
		log:       logger.New(logger.LevelCritical),
	}
	h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		log: h.log,
		scheduledJobs: testScheduledJobOperations{triggerNow: func(
			context.Context,
			string,
			string,
			string,
			secretprovider.SecretProvider,
		) (string, error) {
			return "", errors.New("scheduled trigger exploded")
		}},
	})
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "trigger_scheduled_job", Arguments: map[string]any{"job_name": "backup"}})
	if err != nil {
		t.Fatal(err)
	}

	if !result.IsError {
		t.Fatalf("expected tool error, got %#v", result)
	}

	encodedContent, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(encodedContent), "scheduled trigger exploded") {
		t.Fatalf("error content lacks trigger failure: %s", encodedContent)
	}

	var output triggerScheduledJobOutput

	decodeMCPStructuredContent(t, result, &output)

	if output.JobID == "" || output.Status != string(controlplane.RunStatusFailed) {
		t.Fatalf("structured error output = %#v, want job ID with failed status", output)
	}

	run, ok := h.controlPlaneRuns.Get(output.JobID)
	if !ok || run.Status != controlplane.RunStatusFailed {
		t.Fatalf("job ID from error output did not resolve to a failed run: %#v (found %t)", run, ok)
	}
}

func TestMCPScheduledJobsTool(t *testing.T) {
	skipWithoutLiveDocker(t)

	dockerCli, err := docker.CreateDockerCli(false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	if resolveTestSwarmMode(t, dockerCli.Client()) {
		t.Skip("compose scheduled-job fixture requires standalone mode")
	}

	projectName := test.ConvertTestName(t.Name())
	test.ComposeUp(t.Context(), t, test.WithYAML(`services:
  backup:
    image: alpine:latest
    command: ["sleep", "infinity"]
    labels:
      cd.doco.job.enabled: "true"
      cd.doco.job.schedule: "@every 1h"
`), test.WithName(projectName))

	h := &Handler{dockerCli: dockerCli, log: logger.New(logger.LevelCritical)}
	contexts := docker.NewContextRegistry(dockerCli, docker.ContextRegistryOptions{Quiet: true, SwarmFeatures: true})

	t.Cleanup(func() { _ = contexts.Close() })

	schedulerManager := scheduler.NewManager(contexts, h.log.Logger, nil, nil, nil, nil, docker.ScheduledComposeOptions{})
	h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		dockerCli: dockerCli,
		log:       h.log,
		scheduledJobs: testScheduledJobOperations{listJobs: func(ctx context.Context, _ string, stackName string) ([]scheduler.JobInfo, error) {
			return schedulerManager.ListJobs(ctx, "", stackName)
		}},
	})
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)
	result := callMCPTool(t, session, "list_scheduled_jobs", map[string]any{"stack": " " + projectName + " "})

	var output listScheduledJobsOutput
	decodeMCPStructuredContent(t, result, &output)

	if !containsScheduledJob(output.Jobs, projectName) {
		t.Fatalf("expected scheduled job for stack %q in %#v", projectName, output.Jobs)
	}
}

func containsScheduledJob(jobs []scheduler.JobInfo, stack string) bool {
	for _, job := range jobs {
		if job.Stack == stack {
			return true
		}
	}

	return false
}
