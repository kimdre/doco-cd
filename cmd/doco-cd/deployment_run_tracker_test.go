package main

import (
	"testing"
	"time"
)

func TestDeploymentRunTrackerLifecycle(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(map[deploymentRunTrigger]int{
		deploymentRunTriggerWebhook:      10,
		deploymentRunTriggerPoll:         10,
		deploymentRunTriggerScheduledJob: 10,
	})
	jobID := "job-1"

	tracker.TrackAccepted(jobID, deploymentRunTriggerWebhook)
	tracker.SetMetadata(jobID, "owner/repo", "prod", "refs/heads/main")
	tracker.AddDeployment(jobID, "api", "remote")
	tracker.AddDeployment(jobID, "web", "")
	tracker.AddDeployment(jobID, "api", "remote")
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

	if len(run.Deployments) != 2 {
		t.Fatalf("expected two deployment targets, got %#v", run.Deployments)
	}

	if run.Deployments[0] != (deploymentRunTarget{Stack: "web", Context: "default"}) ||
		run.Deployments[1] != (deploymentRunTarget{Stack: "api", Context: "remote"}) {
		t.Fatalf("unexpected deployment targets: %#v", run.Deployments)
	}
}

func TestDeploymentRunTrackerListAndTrim(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(map[deploymentRunTrigger]int{
		deploymentRunTriggerWebhook:      1,
		deploymentRunTriggerPoll:         2,
		deploymentRunTriggerScheduledJob: 10,
	})

	tracker.TrackAccepted("job-1", deploymentRunTriggerWebhook)
	time.Sleep(time.Millisecond)
	tracker.TrackAccepted("job-2", deploymentRunTriggerPoll)
	time.Sleep(time.Millisecond)
	tracker.TrackAccepted("job-3", deploymentRunTriggerPoll)
	time.Sleep(time.Millisecond)
	tracker.MarkSucceeded("job-1", "done")
	tracker.TrackAccepted("job-4", deploymentRunTriggerWebhook)

	if _, ok := tracker.Get("job-1"); ok {
		t.Fatal("expected oldest terminal webhook run to be evicted")
	}

	// job-4 should exist (latest webhook)
	if _, ok := tracker.Get("job-4"); !ok {
		t.Fatal("expected job-4 (webhook) to exist")
	}

	// both job-2 and job-3 should be present (poll limit is 2)
	runs := tracker.List(10, string(deploymentRunTriggerPoll), "")
	if len(runs) != 2 {
		t.Fatalf("expected 2 poll runs, got %d", len(runs))
	}

	if runs[0].JobID != "job-3" || runs[1].JobID != "job-2" {
		t.Fatalf("expected reverse chronological order, got %q then %q", runs[0].JobID, runs[1].JobID)
	}
}

func TestDeploymentRunTrackerRetainsActiveRunsOverLimit(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(map[deploymentRunTrigger]int{
		deploymentRunTriggerWebhook: 1,
	})

	tracker.TrackAccepted("job-1", deploymentRunTriggerWebhook)
	tracker.MarkRunning("job-1")
	tracker.TrackAccepted("job-2", deploymentRunTriggerWebhook)

	for _, jobID := range []string{"job-1", "job-2"} {
		if _, ok := tracker.Get(jobID); !ok {
			t.Fatalf("expected active run %q to be retained", jobID)
		}
	}
}

func TestDeploymentRunTrackerPrunesOldestTerminalRunAfterTransition(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(map[deploymentRunTrigger]int{
		deploymentRunTriggerWebhook: 1,
	})

	tracker.TrackAccepted("job-1", deploymentRunTriggerWebhook)
	tracker.MarkRunning("job-1")
	tracker.TrackAccepted("job-2", deploymentRunTriggerWebhook)
	tracker.MarkSucceeded("job-1", "done")

	if _, ok := tracker.Get("job-1"); ok {
		t.Fatal("expected oldest terminal run to be pruned")
	}

	if run, ok := tracker.Get("job-2"); !ok || run.Status != deploymentRunStatusAccepted {
		t.Fatalf("expected active run to remain available, got %#v", run)
	}
}

func TestDeploymentRunTrackerRecordsTerminalUpdateBeforePruning(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(map[deploymentRunTrigger]int{
		deploymentRunTriggerWebhook: 2,
	})

	tracker.TrackAccepted("job-0", deploymentRunTriggerWebhook)
	tracker.MarkSucceeded("job-0", "old terminal run")
	tracker.TrackAccepted("job-1", deploymentRunTriggerWebhook)
	tracker.MarkRunning("job-1")
	tracker.TrackAccepted("job-2", deploymentRunTriggerWebhook)
	tracker.MarkSucceeded("job-1", "completed")

	run, ok := tracker.Get("job-1")
	if !ok {
		t.Fatal("expected terminal update to remain available")
	}

	if run.Status != deploymentRunStatusSucceeded || run.Message != "completed" {
		t.Fatalf("terminal update was not recorded: %#v", run)
	}

	if _, ok := tracker.Get("job-0"); ok {
		t.Fatal("expected older terminal run to be pruned")
	}

	if _, ok := tracker.Get("job-2"); !ok {
		t.Fatal("expected accepted job to remain available")
	}
}

func TestDeploymentRunTrackerCleanupRetainsExpiredActiveRuns(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(nil)
	tracker.TrackAccepted("accepted", deploymentRunTriggerWebhook)
	tracker.TrackAccepted("running", deploymentRunTriggerWebhook)
	tracker.MarkRunning("running")

	expiredAt := time.Now().Add(-deploymentRunTTL - time.Hour)

	for _, jobID := range []string{"accepted", "running"} {
		run := tracker.runs[jobID]
		run.CreatedAt = expiredAt
		tracker.runs[jobID] = run
	}

	tracker.cleanup(time.Now())

	for _, jobID := range []string{"accepted", "running"} {
		if _, ok := tracker.Get(jobID); !ok {
			t.Fatalf("expected expired active run %q to be retained", jobID)
		}
	}
}

func TestDeploymentRunTrackerCleanupRemovesExpiredTerminalRuns(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(nil)
	tracker.TrackAccepted("succeeded", deploymentRunTriggerWebhook)
	tracker.MarkSucceeded("succeeded", "done")

	run := tracker.runs["succeeded"]
	run.CreatedAt = time.Now().Add(-deploymentRunTTL - time.Hour)
	tracker.runs["succeeded"] = run

	tracker.cleanup(time.Now())

	if _, ok := tracker.Get("succeeded"); ok {
		t.Fatal("expected expired terminal run to be removed")
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
