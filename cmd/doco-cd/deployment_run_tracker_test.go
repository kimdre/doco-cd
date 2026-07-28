package main

import (
	"testing"
	"time"
)

func TestDeploymentRunTrackerLifecycle(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(10)
	jobID := "job-1"

	tracker.TrackAccepted(jobID, deploymentRunTriggerWebhook)
	tracker.SetMetadata(jobID, "owner/repo", "prod", "refs/heads/main")
	tracker.MarkRunning(jobID)
	tracker.MarkSucceeded(jobID, "done")

	run, ok := tracker.Get(jobID)
	if !ok {
		t.Fatal("expected tracked run")
	}

	if run.Status != deploymentRunStatusSucceeded {
		t.Fatalf("expected status %q, got %q", deploymentRunStatusSucceeded, run.Status)
	}

	if run.Repository != "owner/repo" {
		t.Fatalf("expected repository to be set, got %q", run.Repository)
	}

	if run.StartedAt == nil || run.FinishedAt == nil {
		t.Fatal("expected run timestamps to be set")
	}

	if run.Message != "done" {
		t.Fatalf("expected message to be set, got %q", run.Message)
	}
}

func TestDeploymentRunTrackerListAndTrim(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(2)

	tracker.TrackAccepted("job-1", deploymentRunTriggerWebhook)
	time.Sleep(time.Millisecond)
	tracker.TrackAccepted("job-2", deploymentRunTriggerPoll)
	time.Sleep(time.Millisecond)
	tracker.TrackAccepted("job-3", deploymentRunTriggerPoll)

	if _, ok := tracker.Get("job-1"); ok {
		t.Fatal("expected oldest entry to be evicted")
	}

	runs := tracker.List(10, string(deploymentRunTriggerPoll), "")
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}

	if runs[0].JobID != "job-3" || runs[1].JobID != "job-2" {
		t.Fatalf("expected reverse chronological order, got %q then %q", runs[0].JobID, runs[1].JobID)
	}
}

func TestNormalizeDeploymentRunParams(t *testing.T) {
	t.Parallel()

	if _, err := normalizeDeploymentRunStatus("wat"); err == nil {
		t.Fatal("expected invalid status error")
	}

	if _, err := normalizeDeploymentRunTrigger("wat"); err == nil {
		t.Fatal("expected invalid trigger error")
	}

	trigger, err := normalizeDeploymentRunTrigger("  WEBHOOK ")
	if err != nil || trigger != "webhook" {
		t.Fatalf("unexpected trigger normalization: %q (%v)", trigger, err)
	}

	status, err := normalizeDeploymentRunStatus("  RUNNING ")
	if err != nil || status != "running" {
		t.Fatalf("unexpected status normalization: %q (%v)", status, err)
	}
}
