package controlplane

import (
	"context"
	"testing"

	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

func TestControlPlaneRunsScheduledJobOperations(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(nil)
	listed := false
	triggered := false
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		tracker: tracker,
		scheduledJobs: testScheduledJobOperations{
			listJobs: func(_ context.Context, contextName, stackName string) ([]scheduler.JobInfo, error) {
				listed = true

				if contextName != "remote" || stackName != "prod" {
					t.Fatalf("list arguments = %q, %q", contextName, stackName)
				}

				return []scheduler.JobInfo{{Name: "backup"}}, nil
			},
			triggerNow: func(_ context.Context, contextName, jobName, stackName string, _ *secretprovider.SecretProvider) (string, error) {
				triggered = true

				if contextName != "remote" || jobName != "backup" || stackName != "prod" {
					t.Fatalf("trigger arguments = %q, %q, %q", contextName, jobName, stackName)
				}

				return "scheduled-run", nil
			},
		},
	})

	jobs, err := runs.ListScheduledJobs(t.Context(), "remote", "prod")
	if err != nil {
		t.Fatal(err)
	}

	if !listed || len(jobs) != 1 || jobs[0].Name != "backup" {
		t.Fatalf("jobs = %#v, listed = %t", jobs, listed)
	}

	jobID, err := runs.TriggerScheduledJob(t.Context(), "job", "remote", "backup", "prod", true)
	if err != nil {
		t.Fatal(err)
	}

	run, ok := tracker.Get(jobID)
	if !triggered || !ok || run.Status != deploymentRunStatusSucceeded {
		t.Fatalf("run = %#v, found = %t, triggered = %t", run, ok, triggered)
	}
}
