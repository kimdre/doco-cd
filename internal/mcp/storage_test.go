package mcp

import (
	"context"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
)

// fakeCompactionRuns implements the mirror compaction part of RunOperations.
type fakeCompactionRuns struct {
	RunOperations

	trigger func(ctx context.Context, jobID string, req controlplane.MirrorCompactionRequest, wait bool) (string, error)
	runs    map[string]controlplane.Run
}

func (f *fakeCompactionRuns) TriggerMirrorCompaction(ctx context.Context, jobID string, req controlplane.MirrorCompactionRequest, wait bool) (string, error) {
	return f.trigger(ctx, jobID, req, wait)
}

func (f *fakeCompactionRuns) Get(jobID string) (controlplane.Run, bool) {
	run, ok := f.runs[jobID]

	return run, ok
}

func TestMCPCompactMirrorsDefaultsToAsync(t *testing.T) {
	var (
		gotReq  controlplane.MirrorCompactionRequest
		gotWait bool
	)

	runs := &fakeCompactionRuns{
		trigger: func(_ context.Context, _ string, req controlplane.MirrorCompactionRequest, wait bool) (string, error) {
			gotReq, gotWait = req, wait

			return "job-1", nil
		},
		runs: map[string]controlplane.Run{"job-1": {JobID: "job-1", Status: controlplane.RunStatusRunning}},
	}
	h := &Handler{log: logger.New(logger.LevelCritical), controlPlaneRuns: runs}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)

	result := callMCPTool(t, connectMCPTestClient(t, server), "compact_mirrors", map[string]any{
		"repository": "github.com/acme/app",
		"mode":       "copy",
		"max_size":   0,
	})

	var output compactMirrorsOutput
	decodeMCPStructuredContent(t, result, &output)

	if output.JobID != "job-1" || output.Status != string(controlplane.RunStatusAccepted) || output.Message != "" {
		t.Fatalf("output = %#v", output)
	}

	if gotWait || gotReq.Repository != "github.com/acme/app" || gotReq.Mode != git.MirrorCompactionCopy {
		t.Fatalf("request = %+v, wait = %v", gotReq, gotWait)
	}

	if gotReq.MaxSizeBytes == nil || *gotReq.MaxSizeBytes != 0 {
		t.Fatalf("max size = %v, want 0", gotReq.MaxSizeBytes)
	}
}

func TestMCPCompactMirrorsWaitReportsRun(t *testing.T) {
	runs := &fakeCompactionRuns{
		trigger: func(context.Context, string, controlplane.MirrorCompactionRequest, bool) (string, error) {
			return "job-1", nil
		},
		runs: map[string]controlplane.Run{"job-1": {
			JobID:   "job-1",
			Status:  controlplane.RunStatusSkipped,
			Message: "repack of 1 mirrors: 1 skipped_single_pack; packfiles 1.0 KiB -> 1.0 KiB",
		}},
	}
	h := &Handler{log: logger.New(logger.LevelCritical), controlPlaneRuns: runs}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)

	result := callMCPTool(t, connectMCPTestClient(t, server), "compact_mirrors", map[string]any{"wait": true})

	var output compactMirrorsOutput
	decodeMCPStructuredContent(t, result, &output)

	if output.Status != string(controlplane.RunStatusSkipped) || output.Message != runs.runs["job-1"].Message {
		t.Fatalf("output = %#v", output)
	}
}

func TestMCPCompactMirrorsReportsActiveRun(t *testing.T) {
	runs := &fakeCompactionRuns{
		trigger: func(context.Context, string, controlplane.MirrorCompactionRequest, bool) (string, error) {
			return "active-job", &controlplane.MirrorCompactionActiveError{JobID: "active-job"}
		},
		runs: map[string]controlplane.Run{"active-job": {JobID: "active-job", Status: controlplane.RunStatusRunning}},
	}
	h := &Handler{log: logger.New(logger.LevelCritical), controlPlaneRuns: runs}
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	assertMCPToolError(t, session, "compact_mirrors", map[string]any{}, "already running")

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "compact_mirrors", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}

	var output compactMirrorsOutput
	decodeMCPStructuredContent(t, result, &output)

	if output.JobID != "active-job" || output.Status != string(controlplane.RunStatusRunning) {
		t.Fatalf("output = %#v", output)
	}
}

func TestMCPCompactMirrorsRejectsRequests(t *testing.T) {
	log := logger.New(logger.LevelCritical)
	h := &Handler{log: log}
	h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{log: log, storageDir: t.TempDir()})
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	assertMCPToolError(t, session, "compact_mirrors", map[string]any{"mode": "gc"}, "/properties/mode")
	assertMCPToolError(t, session, "compact_mirrors", map[string]any{"max_size": -1}, "/properties/max_size")
	assertMCPToolError(t, session, "compact_mirrors", map[string]any{"repository": "github.com/acme/app"}, "no git mirrors found for repository github.com/acme/app")

	if got := h.controlPlaneRuns.List(10, string(controlplane.RunTriggerMirrorCompaction), ""); len(got) != 0 {
		t.Fatalf("rejected compactions were tracked: %#v", got)
	}
}
