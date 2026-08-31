package mcp

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/logger"
)

func TestMCPDeploymentRunTools(t *testing.T) {
	h := &Handler{log: logger.New(logger.LevelCritical)}
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{log: h.log})
	h.controlPlaneRuns = runs
	succeedTestRun(t, runs, "job-webhook", controlplane.RunTriggerWebhook, "complete")
	runs.Accept("job-poll", controlplane.RunTriggerPoll, controlplane.RunMetadata{})

	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	t.Run("list defaults and normalized filters", func(t *testing.T) {
		result := callMCPTool(t, session, "list_deployment_runs", map[string]any{
			"limit":   1,
			"status":  " SUCCEEDED ",
			"trigger": " WEBHOOK ",
		})

		var output listDeploymentRunsOutput
		decodeMCPStructuredContent(t, result, &output)

		if len(output.Runs) != 1 || output.Runs[0].JobID != "job-webhook" {
			t.Fatalf("unexpected filtered runs: %#v", output.Runs)
		}
	})

	t.Run("list defaults to 50", func(t *testing.T) {
		defaultHandler := &Handler{log: logger.New(logger.LevelCritical)}

		runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			log: defaultHandler.log,
			maxRunsPerTrigger: map[controlplane.RunTrigger]int{
				controlplane.RunTriggerWebhook: 60,
			},
		})

		defaultHandler.controlPlaneRuns = runs
		for i := range 60 {
			runs.Accept("default-job-"+strconv.Itoa(i), controlplane.RunTriggerWebhook, controlplane.RunMetadata{})
		}

		defaultServer, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, defaultHandler)
		result := callMCPTool(t, connectMCPTestClient(t, defaultServer), "list_deployment_runs", map[string]any{})

		var output listDeploymentRunsOutput
		decodeMCPStructuredContent(t, result, &output)

		if len(output.Runs) != 50 {
			t.Fatalf("expected default limit 50, got %d runs", len(output.Runs))
		}
	})

	t.Run("get run", func(t *testing.T) {
		result := callMCPTool(t, session, "get_deployment_run", map[string]any{"job_id": " job-webhook "})

		var output getDeploymentRunOutput
		decodeMCPStructuredContent(t, result, &output)

		if output.Run.JobID != "job-webhook" || output.Run.Status != controlplane.RunStatusSucceeded {
			t.Fatalf("unexpected run: %#v", output.Run)
		}
	})

	for _, testCase := range []struct {
		name      string
		tool      string
		arguments map[string]any
		contains  string
	}{
		{name: "zero limit", tool: "list_deployment_runs", arguments: map[string]any{"limit": 0}, contains: "less than 1"},
		{name: "negative limit", tool: "list_deployment_runs", arguments: map[string]any{"limit": -1}, contains: "less than 1"},
		{name: "invalid status", tool: "list_deployment_runs", arguments: map[string]any{"status": "unknown"}, contains: "invalid deployment run status"},
		{name: "invalid trigger", tool: "list_deployment_runs", arguments: map[string]any{"trigger": "unknown"}, contains: "invalid deployment run trigger"},
		{name: "missing job id", tool: "get_deployment_run", arguments: map[string]any{}, contains: "job_id"},
		{name: "blank job id", tool: "get_deployment_run", arguments: map[string]any{"job_id": "  "}, contains: "missing job id"},
		{name: "run not found", tool: "get_deployment_run", arguments: map[string]any{"job_id": "missing"}, contains: "run not found: missing"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			assertMCPToolError(t, session, testCase.tool, testCase.arguments, testCase.contains)
		})
	}
}

func TestMCPListDeploymentRunsCapsLimitAt200(t *testing.T) {
	h := &Handler{log: logger.New(logger.LevelCritical)}

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		log: h.log,
		maxRunsPerTrigger: map[controlplane.RunTrigger]int{
			controlplane.RunTriggerWebhook: 250,
		},
	})

	h.controlPlaneRuns = runs
	for i := range 205 {
		runs.Accept("job-"+strconv.Itoa(i), controlplane.RunTriggerWebhook, controlplane.RunMetadata{})
	}

	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	result := callMCPTool(t, connectMCPTestClient(t, server), "list_deployment_runs", map[string]any{"limit": 201})

	var output listDeploymentRunsOutput
	decodeMCPStructuredContent(t, result, &output)

	if len(output.Runs) != 200 {
		t.Fatalf("expected limit to be capped at 200, got %d runs", len(output.Runs))
	}
}

func succeedTestRun(
	t testing.TB,
	runs *controlplane.Runs,
	jobID string,
	trigger controlplane.RunTrigger,
	message string,
) {
	t.Helper()

	runs.Accept(jobID, trigger, controlplane.RunMetadata{})

	if err := runs.Execute(t.Context(), jobID, controlplane.RunExecution{
		Mode:         controlplane.RunSynchronous,
		PanicContext: "test run",
		PanicError:   errors.New("test run panicked"),
	}, func(context.Context) (controlplane.RunResult, error) {
		return controlplane.SucceededRun(message), nil
	}); err != nil {
		t.Fatal(err)
	}
}
