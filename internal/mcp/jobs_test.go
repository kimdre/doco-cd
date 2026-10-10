package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/test"
)

func TestMCPScheduledJobsLastRunContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status controlplane.RunStatus
		runID  string
		evict  bool
	}{
		{name: "never run"},
		{name: "unavailable history", runID: "missing-run"},
		{name: "evicted history", status: controlplane.RunStatusSucceeded, runID: "evicted-run", evict: true},
		{name: "running", status: controlplane.RunStatusRunning, runID: "running-run"},
		{name: "succeeded", status: controlplane.RunStatusSucceeded, runID: "successful-run"},
		{name: "failed", status: controlplane.RunStatusFailed, runID: "failed-run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dockerCli, err := command.NewDockerCli()
			if err != nil {
				t.Fatal(err)
			}

			startedAt := time.Now().In(time.Local).Add(-time.Minute)
			nextRunAt := startedAt.Add(time.Hour)
			job := scheduler.JobInfo{
				LatestRunID: tc.runID, Name: "backup", Context: "default", Stack: "prod", Mode: "container",
				Schedule: "@every 1h", ExecutionMode: docker.JobExecutionModeOneOff, NotifyOn: docker.JobNotifyAll,
				Status: "exited (0)", Repository: "owner/repo", StopServices: []string{"app"}, Replicas: 1,
				Enabled: true, SkipRunning: true, Valid: true, LastRunAt: &startedAt,
				NextRunAt: &nextRunAt, LabelNextRunAt: &nextRunAt,
			}

			runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				maxRunsPerTrigger: map[controlplane.RunTrigger]int{controlplane.RunTriggerScheduledJob: 1},
				scheduledJobs: testScheduledJobOperations{listJobs: func(_ context.Context, contextName, stackName string) ([]scheduler.JobInfo, error) {
					if contextName != "" || stackName != "prod" {
						t.Errorf("list filters = %q, %q", contextName, stackName)
					}

					return []scheduler.JobInfo{job}, nil
				}},
			})
			if tc.status != "" {
				runs.ScheduledRunStarted(scheduler.RunExecution{
					RunID: tc.runID, JobName: job.Name, Context: job.Context, Stack: job.Stack, Mode: job.Mode, StartedAt: startedAt,
				})
				runs.SetMetadata(tc.runID, controlplane.RunMetadata{Repository: job.Repository, Target: job.Stack, Revision: "main"})

				switch tc.status {
				case controlplane.RunStatusSucceeded:
					runs.ScheduledRunFinished(tc.runID, nil)
				case controlplane.RunStatusFailed:
					runs.ScheduledRunFinished(tc.runID, errors.New("exit code 7"))
				}
			}

			if tc.evict {
				runs.ScheduledRunStarted(scheduler.RunExecution{RunID: "newer-run", JobName: "other", StartedAt: time.Now().UTC()})
				runs.ScheduledRunFinished("newer-run", nil)

				if _, ok := runs.Get(tc.runID); ok {
					t.Fatal("eviction fixture still has the original run")
				}
			}

			h := &Handler{dockerCli: dockerCli, log: logger.New(logger.LevelCritical), controlPlaneRuns: runs}
			server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
			session := connectMCPTestClient(t, server)
			result := callMCPTool(t, session, "list_scheduled_jobs", map[string]any{"stack": " prod ", "context": "default"})

			var output struct {
				Jobs []map[string]json.RawMessage `json:"jobs"`
			}
			decodeMCPStructuredContent(t, result, &output)

			if len(output.Jobs) != 1 {
				t.Fatalf("jobs = %#v, want one job", output.Jobs)
			}

			lastRun, present := output.Jobs[0]["last_run"]
			if !present {
				t.Fatal("last_run must always be serialized")
			}

			if tc.status == "" || tc.evict {
				if string(lastRun) != "null" {
					t.Fatalf("last_run = %s, want null even when last_run_at exists", lastRun)
				}
			} else {
				detail := callMCPTool(t, session, "get_deployment_run", map[string]any{"job_id": tc.runID})

				var detailOutput map[string]json.RawMessage
				decodeMCPStructuredContent(t, detail, &detailOutput)

				if !reflect.DeepEqual(lastRun, detailOutput["run"]) {
					t.Fatalf("last_run %s differs from complete run detail %s", lastRun, detailOutput["run"])
				}
			}

			delete(output.Jobs[0], "last_run")

			encodedJob, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}

			var original map[string]json.RawMessage
			if err := json.Unmarshal(encodedJob, &original); err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(output.Jobs[0], original) {
				t.Fatalf("existing flat job fields changed: got %#v, want %#v", output.Jobs[0], original)
			}

			for _, hidden := range []string{"LatestRunID", "latest_run_id", "last_run_id", "JobInfo"} {
				if _, ok := output.Jobs[0][hidden]; ok {
					t.Fatalf("job leaked internal or redundant field %q", hidden)
				}
			}
		})
	}
}

func TestMCPScheduledJobsLastRunOutputSchema(t *testing.T) {
	dockerCli, err := command.NewDockerCli()
	if err != nil {
		t.Fatal(err)
	}

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		scheduledJobs: testScheduledJobOperations{listJobs: func(context.Context, string, string) ([]scheduler.JobInfo, error) {
			return []scheduler.JobInfo{
				{Name: "backup", Context: "default", Mode: "container", Enabled: true, Valid: true, LatestRunID: "completed-run"},
				{Name: "never-run", Context: "default", Mode: "container", Enabled: true, Valid: true},
			}, nil
		}},
	})
	runs.ScheduledRunStarted(scheduler.RunExecution{RunID: "completed-run", JobName: "backup", StartedAt: time.Now().UTC()})
	runs.ScheduledRunFinished("completed-run", nil)
	h := &Handler{dockerCli: dockerCli, log: logger.New(logger.LevelCritical), controlPlaneRuns: runs}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	result, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	var jobsSchema, detailSchema *jsonschema.Schema

	for _, tool := range result.Tools {
		if tool.Name != "list_scheduled_jobs" && tool.Name != "get_deployment_run" {
			continue
		}

		encoded, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatal(err)
		}

		var schema jsonschema.Schema
		if err := json.Unmarshal(encoded, &schema); err != nil {
			t.Fatal(err)
		}

		if tool.Name == "list_scheduled_jobs" {
			jobsSchema = &schema
		} else {
			detailSchema = &schema
		}
	}

	if jobsSchema == nil || detailSchema == nil {
		t.Fatal("scheduled-job and run-detail output schemas must be advertised")
	}

	job := jobsSchema.Properties["jobs"].Items
	for _, name := range []string{"name", "context", "stack", "mode", "enabled", "valid", "last_run_at", "next_run_at", "last_run"} {
		if job.Properties[name] == nil {
			t.Errorf("flat job schema missing %q", name)
		}
	}

	if !slices.Contains(job.Required, "last_run") {
		t.Fatal("last_run must be required, including when null")
	}

	lastRun := job.Properties["last_run"]
	if lastRun == nil || !slices.Contains(lastRun.Types, "null") || !slices.Contains(lastRun.Types, "object") {
		t.Fatalf("last_run must be a nullable run object: %#v", lastRun)
	}

	if !reflect.DeepEqual(lastRun.Properties, detailSchema.Properties["run"].Properties) {
		t.Fatal("last_run schema differs from complete run-detail schema")
	}

	if !slices.Equal(lastRun.Properties["status"].Enum, []any{"accepted", "running", "succeeded", "failed", "skipped"}) {
		t.Fatalf("run status enum = %#v", lastRun.Properties["status"].Enum)
	}

	if !slices.Equal(lastRun.Properties["trigger"].Enum, []any{"webhook", "poll", "scheduled_job", "mirror_compaction"}) {
		t.Fatalf("run trigger enum = %#v", lastRun.Properties["trigger"].Enum)
	}

	for _, hidden := range []string{"LatestRunID", "latest_run_id", "last_run_id", "JobInfo"} {
		if job.Properties[hidden] != nil {
			t.Errorf("job schema leaked internal or redundant field %q", hidden)
		}
	}

	resolved, err := jobsSchema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve advertised output schema: %v", err)
	}

	output := callMCPTool(t, session, "list_scheduled_jobs", map[string]any{})

	var structured map[string]any
	decodeMCPStructuredContent(t, output, &structured)

	if err := resolved.Validate(structured); err != nil {
		t.Fatalf("advertised output schema rejects full run or explicit null: %v", err)
	}

	jobs := structured["jobs"].([]any)
	neverRun := jobs[1].(map[string]any)
	delete(neverRun, "last_run")

	if err := resolved.Validate(structured); err == nil {
		t.Fatal("advertised output schema accepts an omitted last_run")
	}

	neverRun["last_run"] = nil
	jobs[0].(map[string]any)["last_run"].(map[string]any)["status"] = "unknown"

	if err := resolved.Validate(structured); err == nil {
		t.Fatal("advertised output schema accepts an unknown run status")
	}
}

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

func containsScheduledJob(jobs []controlplane.ScheduledJobInfo, stack string) bool {
	for _, job := range jobs {
		if job.Stack == stack {
			return true
		}
	}

	return false
}
