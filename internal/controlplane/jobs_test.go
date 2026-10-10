package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

func TestScheduledJobAndRunTimestampsUseLocalTimezone(t *testing.T) {
	location, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}

	originalLocal := time.Local
	time.Local = location

	t.Cleanup(func() { time.Local = originalLocal })

	for _, testCase := range []struct {
		name   string
		at     time.Time
		offset string
	}{
		{name: "summer", at: time.Date(2026, time.July, 1, 12, 0, 0, 123, time.UTC), offset: "+02:00"},
		{name: "winter", at: time.Date(2026, time.January, 1, 12, 0, 0, 123, time.UTC), offset: "+01:00"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			at := testCase.at
			tracker := newDeploymentRunTracker(nil)
			tracker.upsert(Run{
				JobID: "selected", Trigger: RunTriggerScheduledJob, Status: RunStatusRunning,
				CreatedAt: at, StartedAt: &at, FinishedAt: &at, UpdatedAt: at,
			})
			runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				tracker: tracker,
				scheduledJobs: testScheduledJobOperations{
					listJobs: func(context.Context, string, string) ([]scheduler.JobInfo, error) {
						return []scheduler.JobInfo{{
							Name: "backup", LatestRunID: "selected",
							LastRunAt: &at, NextRunAt: &at, LabelNextRunAt: &at,
						}}, nil
					},
				},
			})

			jobs, err := runs.ListScheduledJobs(t.Context(), "", "")
			if err != nil || len(jobs) != 1 || jobs[0].LastRun == nil {
				t.Fatalf("jobs = %#v, error = %v", jobs, err)
			}

			detail, ok := runs.Get("selected")

			history := runs.List(1, "", "")
			if !ok || len(history) != 1 || !reflect.DeepEqual(detail, *jobs[0].LastRun) || !reflect.DeepEqual(detail, history[0]) {
				t.Fatal("job, run-detail, and run-history timestamps differ")
			}

			for _, timestamp := range []*time.Time{
				jobs[0].LastRunAt, jobs[0].NextRunAt, jobs[0].LabelNextRunAt,
				&detail.CreatedAt, detail.StartedAt, detail.FinishedAt, &detail.UpdatedAt,
			} {
				if timestamp == nil || timestamp.Location() != location || !timestamp.Equal(at) {
					t.Fatalf("timestamp = %v, want the same instant in Europe/Berlin", timestamp)
				}
			}

			data, err := json.Marshal(jobs[0])
			if err != nil {
				t.Fatal(err)
			}

			if strings.Count(string(data), testCase.offset+`"`) != 7 {
				t.Fatalf("all seven JSON timestamps must use %s: %s", testCase.offset, data)
			}

			*jobs[0].LastRunAt = at.Add(time.Hour)
			*detail.StartedAt = at.Add(time.Hour)

			stored := tracker.runs["selected"]
			if !at.Equal(testCase.at) || stored.CreatedAt.Location() != time.UTC ||
				stored.StartedAt.Location() != time.UTC || !stored.StartedAt.Equal(testCase.at) {
				t.Fatal("display conversion mutated the original timestamps")
			}
		})
	}
}

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
			triggerNow: func(_ context.Context, contextName, jobName, stackName string, _ secretprovider.SecretProvider) (string, error) {
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

func TestListScheduledJobsEnrichesSelectedRun(t *testing.T) {
	t.Parallel()

	startedAt := time.Now().Add(-time.Hour).In(time.Local)
	lastRunAt := startedAt
	nextRunAt := startedAt.Add(2 * time.Hour)
	job := scheduler.JobInfo{
		LatestRunID:    "latest",
		LastRunAt:      &lastRunAt,
		NextRunAt:      &nextRunAt,
		LabelNextRunAt: &nextRunAt,
		Name:           "backup",
		Context:        "remote",
		Stack:          "prod",
		Mode:           "swarm",
		Schedule:       "0 * * * *",
		ExecutionMode:  docker.JobExecutionModeOneOff,
		Status:         "completed",
		Repository:     "owner/repo",
		StopServices:   []string{"db"},
		Replicas:       1,
		Enabled:        true,
		SkipRunning:    true,
		Valid:          true,
	}
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		scheduledJobs: testScheduledJobOperations{
			listJobs: func(context.Context, string, string) ([]scheduler.JobInfo, error) {
				return []scheduler.JobInfo{job, {Name: "never-run", LastRunAt: &lastRunAt}}, nil
			},
		},
	})
	runs.ScheduledRunStarted(scheduler.RunExecution{
		RunID:     "latest",
		JobName:   job.Name,
		Context:   job.Context,
		Stack:     job.Stack,
		Mode:      job.Mode,
		StartedAt: startedAt,
	})

	for _, finish := range []bool{false, true} {
		if finish {
			runs.ScheduledRunFinished("latest", nil)
		}

		jobs, err := runs.ListScheduledJobs(t.Context(), "", "")
		if err != nil {
			t.Fatal(err)
		}

		detail, ok := runs.Get("latest")
		if !ok || len(jobs) != 2 || !reflect.DeepEqual(jobs[0].JobInfo, job) || !reflect.DeepEqual(jobs[0].LastRun, &detail) {
			t.Fatalf("jobs = %#v, detail = %#v, found = %t", jobs, detail, ok)
		}

		if jobs[1].LastRun != nil {
			t.Fatalf("last_run inferred from last_run_at: %#v", jobs[1])
		}

		data, err := json.Marshal(jobs)
		if err != nil {
			t.Fatal(err)
		}

		var objects []map[string]json.RawMessage
		if err := json.Unmarshal(data, &objects); err != nil {
			t.Fatal(err)
		}

		detailJSON, err := json.Marshal(detail)
		if err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(objects[0]["last_run"], detailJSON) {
			t.Fatalf("last_run = %s, detail = %s", objects[0]["last_run"], detailJSON)
		}

		if string(objects[1]["last_run"]) != "null" {
			t.Fatalf("never-run last_run = %s, want null", objects[1]["last_run"])
		}

		rawJobJSON, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}

		var rawFields map[string]json.RawMessage
		if err := json.Unmarshal(rawJobJSON, &rawFields); err != nil {
			t.Fatal(err)
		}

		delete(objects[0], "last_run")

		if !reflect.DeepEqual(objects[0], rawFields) {
			t.Fatalf("scheduled job fields changed: got %s, want %s", data, rawJobJSON)
		}

		for _, field := range []string{"LatestRunID", "latest_run_id", "last_run_id", "JobInfo"} {
			if _, leaked := objects[0][field]; leaked {
				t.Fatalf("internal field %q leaked", field)
			}
		}
	}
}

func TestListScheduledJobsPreservesDiscoveryResults(t *testing.T) {
	t.Parallel()

	discoveryErr := errors.New("discovery failed")
	for _, testCase := range []struct {
		name string
		jobs []scheduler.JobInfo
		err  error
	}{
		{name: "nil"},
		{name: "empty", jobs: []scheduler.JobInfo{}},
		{name: "discovery error", err: discoveryErr},
		{name: "empty with error", jobs: []scheduler.JobInfo{}, err: discoveryErr},
		{name: "partial discovery", jobs: []scheduler.JobInfo{{Name: "second"}, {Name: "first"}}, err: discoveryErr},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				scheduledJobs: testScheduledJobOperations{
					listJobs: func(ctx context.Context, contextName, stackName string) ([]scheduler.JobInfo, error) {
						if ctx != t.Context() || contextName != "remote" || stackName != "prod" {
							t.Fatalf("discovery arguments = %v, %q, %q", ctx, contextName, stackName)
						}

						return testCase.jobs, testCase.err
					},
				},
			})

			jobs, err := runs.ListScheduledJobs(t.Context(), "remote", "prod")
			if err != testCase.err || (jobs == nil) != (testCase.jobs == nil) || len(jobs) != len(testCase.jobs) {
				t.Fatalf("ListScheduledJobs() = %#v, %v; want %#v, %v", jobs, err, testCase.jobs, testCase.err)
			}

			for i := range jobs {
				if !reflect.DeepEqual(jobs[i].JobInfo, testCase.jobs[i]) || jobs[i].LastRun != nil {
					t.Fatalf("job %d changed: %#v", i, jobs[i])
				}
			}
		})
	}
}

func TestListScheduledJobsLooksBeyondHistoryPage(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(map[RunTrigger]int{RunTriggerScheduledJob: 100})
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		tracker: tracker,
		scheduledJobs: testScheduledJobOperations{
			listJobs: func(context.Context, string, string) ([]scheduler.JobInfo, error) {
				return []scheduler.JobInfo{{Name: "backup", LatestRunID: "selected"}}, nil
			},
		},
	})
	runs.ScheduledRunStarted(scheduler.RunExecution{RunID: "selected", JobName: "backup"})
	runs.ScheduledRunFinished("selected", nil)

	for i := range 60 {
		runID := fmt.Sprintf("other-%d", i)
		runs.ScheduledRunStarted(scheduler.RunExecution{RunID: runID, JobName: "other"})
		runs.ScheduledRunFinished(runID, nil)
	}

	history := runs.List(50, string(RunTriggerScheduledJob), "")
	if len(history) != 50 || slices.ContainsFunc(history, func(run Run) bool { return run.JobID == "selected" }) {
		t.Fatalf("selected run unexpectedly in history page: %#v", history)
	}

	jobs, err := runs.ListScheduledJobs(t.Context(), "", "")
	if err != nil || len(jobs) != 1 || jobs[0].LastRun == nil || jobs[0].LastRun.JobID != "selected" {
		t.Fatalf("ListScheduledJobs() = %#v, %v", jobs, err)
	}
}

func TestListScheduledJobsAppliesTTL(t *testing.T) {
	t.Parallel()

	expiredAt := time.Now().Add(-deploymentRunTTL - time.Hour)
	tracker := newDeploymentRunTracker(nil)
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		tracker: tracker,
		scheduledJobs: testScheduledJobOperations{
			listJobs: func(context.Context, string, string) ([]scheduler.JobInfo, error) {
				return []scheduler.JobInfo{
					{Name: "active", LatestRunID: "active"},
					{Name: "expired", LatestRunID: "expired", LastRunAt: &expiredAt},
				}, nil
			},
		},
	})
	runs.ScheduledRunStarted(scheduler.RunExecution{RunID: "active", JobName: "active", StartedAt: expiredAt})
	runs.ScheduledRunStarted(scheduler.RunExecution{RunID: "expired", JobName: "expired", StartedAt: expiredAt})
	runs.ScheduledRunFinished("expired", nil)

	if _, ok := tracker.Get("expired"); !ok {
		t.Fatal("expired terminal record removed before job listing")
	}

	jobs, err := runs.ListScheduledJobs(t.Context(), "", "")
	if err != nil || len(jobs) != 2 || jobs[0].LastRun == nil || jobs[1].LastRun != nil {
		t.Fatalf("ListScheduledJobs() = %#v, %v", jobs, err)
	}

	if _, ok := tracker.Get("expired"); ok {
		t.Fatal("job listing did not clean up expired terminal record")
	}

	if jobs[0].LastRun.Status != RunStatusRunning {
		t.Fatalf("expired active run was not retained: %#v", jobs[0].LastRun)
	}
}

func TestListScheduledJobsDoesNotFallBackAfterEviction(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(map[RunTrigger]int{RunTriggerScheduledJob: 2})
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		tracker: tracker,
		scheduledJobs: testScheduledJobOperations{
			listJobs: func(context.Context, string, string) ([]scheduler.JobInfo, error) {
				return []scheduler.JobInfo{
					{Name: "backup", LatestRunID: "selected"},
					{Name: "missing", LatestRunID: "not-tracked"},
				}, nil
			},
		},
	})
	runs.ScheduledRunStarted(scheduler.RunExecution{RunID: "older", JobName: "backup"})
	runs.ScheduledRunStarted(scheduler.RunExecution{RunID: "selected", JobName: "backup"})
	runs.ScheduledRunFinished("selected", nil)
	runs.ScheduledRunStarted(scheduler.RunExecution{RunID: "newest", JobName: "other"})

	if _, ok := tracker.Get("selected"); ok {
		t.Fatal("selected terminal record was not evicted")
	}

	if _, ok := tracker.Get("older"); !ok {
		t.Fatal("older active run was evicted")
	}

	jobs, err := runs.ListScheduledJobs(t.Context(), "", "")
	if err != nil || len(jobs) != 2 || jobs[0].LastRun != nil || jobs[1].LastRun != nil {
		t.Fatalf("ListScheduledJobs() = %#v, %v", jobs, err)
	}
}

func TestScheduledRunStartedRestoresOriginalTime(t *testing.T) {
	t.Parallel()

	originalTime := time.Now().Add(-2 * time.Hour).In(time.FixedZone("original", 3*60*60))
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})
	runs.ScheduledRunStarted(scheduler.RunExecution{
		RunID:     "recovered",
		JobName:   " backup ",
		Context:   " DEFAULT ",
		Stack:     " prod ",
		Mode:      "container",
		StartedAt: originalTime,
	})

	run, ok := runs.Get("recovered")
	if !ok || run.JobID != "recovered" || run.Trigger != RunTriggerScheduledJob || run.Status != RunStatusRunning {
		t.Fatalf("recovered run = %#v, found = %t", run, ok)
	}

	if !run.CreatedAt.Equal(originalTime) || run.StartedAt == nil || !run.StartedAt.Equal(originalTime) {
		t.Fatalf("original time not restored: %#v", run)
	}

	if run.CreatedAt.Location() != time.Local || run.StartedAt.Location() != time.Local || run.UpdatedAt.Before(originalTime) || run.FinishedAt != nil {
		t.Fatalf("recovered timestamps = %#v", run)
	}

	if run.Repository != "scheduled:backup" || run.Target != "prod" ||
		!reflect.DeepEqual(run.Deployments, []RunTarget{{Stack: "prod", Context: "default"}}) {
		t.Fatalf("resolved metadata = %#v", run)
	}

	runs.ScheduledRunStarted(scheduler.RunExecution{RunID: "recovered", JobName: "backup", StartedAt: originalTime.Add(time.Hour)})

	repeated, _ := runs.Get("recovered")
	if !repeated.CreatedAt.Equal(originalTime) || !repeated.StartedAt.Equal(originalTime) || len(runs.List(0, "", "")) != 1 {
		t.Fatalf("repeated start reset or duplicated the run: %#v", repeated)
	}
}

func TestScheduledRunStartedPreservesAdmission(t *testing.T) {
	t.Parallel()

	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("running=%t", running), func(t *testing.T) {
			runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})
			runs.Accept("manual", RunTriggerScheduledJob, RunMetadata{
				Repository: "scheduled:requested",
				Target:     "requested",
				Revision:   "keep-revision",
			})
			runs.AddDeployment("manual", "requested", "default")

			if running {
				runs.MarkRunning("manual")
			}

			admitted, _ := runs.Get("manual")
			startedAt := admitted.CreatedAt.Add(time.Hour)
			runs.ScheduledRunStarted(scheduler.RunExecution{
				RunID:     "manual",
				JobName:   "resolved",
				Context:   " remote ",
				Stack:     " prod ",
				StartedAt: startedAt,
			})

			run, ok := runs.Get("manual")
			if !ok || run.Status != RunStatusRunning || run.Trigger != RunTriggerScheduledJob ||
				!run.CreatedAt.Equal(admitted.CreatedAt) || run.Revision != admitted.Revision {
				t.Fatalf("admission metadata changed: before %#v, after %#v", admitted, run)
			}

			expectedStart := &startedAt
			if running {
				expectedStart = admitted.StartedAt
			}

			if run.StartedAt == nil || !run.StartedAt.Equal(*expectedStart) || run.FinishedAt != nil {
				t.Fatalf("start time reset: before %#v, after %#v", admitted, run)
			}

			if run.Repository != "scheduled:resolved" || run.Target != "prod" ||
				!reflect.DeepEqual(run.Deployments, []RunTarget{{Stack: "prod", Context: "remote"}}) {
				t.Fatalf("provisional metadata not replaced: %#v", run)
			}

			if len(runs.List(0, "", "")) != 1 {
				t.Fatal("start created a second manual run")
			}
		})
	}
}

func TestScheduledRunLifecycleIsTerminalIdempotent(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		err    error
		status RunStatus
	}{
		{name: "success", status: RunStatusSucceeded},
		{name: "failure", err: errors.New("launch failed"), status: RunStatusFailed},
		{name: "cancelled", err: context.Canceled, status: RunStatusFailed},
		{name: "panicked", err: scheduler.ErrScheduledRunPanicked, status: RunStatusFailed},
		{name: "skipped", status: RunStatusSkipped},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})
			runs.ScheduledRunStarted(scheduler.RunExecution{RunID: "run", JobName: "backup", Stack: "prod"})

			if testCase.status == RunStatusSkipped {
				runs.MarkSkipped("run", "nothing to do")
			} else {
				runs.ScheduledRunFinished("run", testCase.err)
			}

			terminal, ok := runs.Get("run")
			if !ok || terminal.Status != testCase.status || terminal.StartedAt == nil || terminal.FinishedAt == nil {
				t.Fatalf("terminal run = %#v, found = %t", terminal, ok)
			}

			if testCase.err != nil && terminal.Message != testCase.err.Error() {
				t.Fatalf("failure message = %q, want %q", terminal.Message, testCase.err.Error())
			}

			runs.ScheduledRunStarted(scheduler.RunExecution{
				RunID:     "run",
				JobName:   "different",
				Context:   "remote",
				Stack:     "different",
				StartedAt: terminal.CreatedAt.Add(time.Hour),
			})
			runs.ScheduledRunFinished("run", errors.New("cleanup failed"))
			runs.ScheduledRunFinished("run", nil)

			repeated, _ := runs.Get("run")
			if !reflect.DeepEqual(repeated, terminal) {
				t.Fatalf("terminal run changed: before %#v, after %#v", terminal, repeated)
			}
		})
	}
}

func TestScheduledRunLifecycleIgnoresMissingIDs(t *testing.T) {
	t.Parallel()

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})
	runs.ScheduledRunStarted(scheduler.RunExecution{JobName: "backup"})
	runs.ScheduledRunFinished("not-started", nil)

	if history := runs.List(0, "", ""); len(history) != 0 {
		t.Fatalf("fabricated history from missing IDs: %#v", history)
	}
}

type runReportingScheduledJobOperations struct {
	testScheduledJobOperations
	triggerRun func(context.Context, string, string, string, string, secretprovider.SecretProvider) (string, error)
}

func (f runReportingScheduledJobOperations) TriggerNow(
	ctx context.Context,
	runID, contextName, jobName, stackName string,
	provider secretprovider.SecretProvider,
) (string, error) {
	return f.triggerRun(ctx, runID, contextName, jobName, stackName, provider)
}

func TestTriggerScheduledJobUsesOneRunID(t *testing.T) {
	t.Parallel()

	for _, requestedID := range []string{"manual", ""} {
		t.Run(fmt.Sprintf("requested=%q", requestedID), func(t *testing.T) {
			var (
				runs       *Runs
				receivedID string
			)

			runs = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				scheduledJobs: runReportingScheduledJobOperations{
					triggerRun: func(_ context.Context, runID, contextName, jobName, stackName string, _ secretprovider.SecretProvider) (string, error) {
						if runID == "" || contextName != "" || jobName != "backup" || stackName != "" {
							t.Fatalf("trigger arguments = %q, %q, %q, %q", runID, contextName, jobName, stackName)
						}

						receivedID = runID

						admitted, ok := runs.Get(runID)
						if !ok || admitted.Status != RunStatusRunning {
							t.Fatalf("manual execution not admitted: %#v, found = %t", admitted, ok)
						}

						runs.ScheduledRunStarted(scheduler.RunExecution{
							RunID:     runID,
							JobName:   "resolved-backup",
							Context:   "remote",
							Stack:     "prod",
							StartedAt: admitted.CreatedAt.Add(time.Hour),
						})

						started, _ := runs.Get(runID)
						if started.Status != RunStatusRunning || !started.CreatedAt.Equal(admitted.CreatedAt) ||
							!started.StartedAt.Equal(*admitted.StartedAt) || started.FinishedAt != nil {
							t.Fatalf("manual admission was reset or finalized early: before %#v, after %#v", admitted, started)
						}

						return runID, nil
					},
				},
			})

			jobID, err := runs.TriggerScheduledJob(t.Context(), requestedID, "", "backup", "", true)
			if err != nil || jobID != receivedID || (requestedID != "" && jobID != requestedID) {
				t.Fatalf("TriggerScheduledJob() = %q, %v; scheduler ID = %q", jobID, err, receivedID)
			}

			run, ok := runs.Get(jobID)
			if !ok || run.Status != RunStatusSucceeded || run.Message != "scheduled job trigger completed" ||
				run.Repository != "scheduled:resolved-backup" || run.Target != "prod" ||
				!reflect.DeepEqual(run.Deployments, []RunTarget{{Stack: "prod", Context: "remote"}}) {
				t.Fatalf("manual run = %#v, found = %t", run, ok)
			}

			if history := runs.List(0, "", ""); len(history) != 1 || history[0].JobID != receivedID {
				t.Fatalf("manual run had multiple IDs: %#v", history)
			}
		})
	}
}

func TestScheduledJobRunSnapshotsAreDefensive(t *testing.T) {
	t.Parallel()

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		scheduledJobs: testScheduledJobOperations{
			listJobs: func(context.Context, string, string) ([]scheduler.JobInfo, error) {
				return []scheduler.JobInfo{
					{Name: "backup", LatestRunID: "run"},
					{Name: "same-record", LatestRunID: "run"},
				}, nil
			},
		},
	})
	runs.ScheduledRunStarted(scheduler.RunExecution{RunID: "run", JobName: "backup", Stack: "prod"})
	runs.ScheduledRunFinished("run", nil)
	original, _ := runs.Get("run")

	for _, query := range []string{"get", "list", "jobs"} {
		t.Run(query, func(t *testing.T) {
			var snapshot Run

			switch query {
			case "get":
				snapshot, _ = runs.Get("run")
			case "list":
				snapshot = runs.List(0, "", "")[0]
			case "jobs":
				jobs, err := runs.ListScheduledJobs(t.Context(), "", "")
				if err != nil {
					t.Fatal(err)
				}

				snapshot = *jobs[0].LastRun
				if snapshot.StartedAt == jobs[1].LastRun.StartedAt || snapshot.FinishedAt == jobs[1].LastRun.FinishedAt {
					t.Fatal("job entries share timestamp pointers")
				}
			}

			snapshot.Deployments[0].Stack = "mutated"
			*snapshot.StartedAt = time.Time{}
			*snapshot.FinishedAt = time.Time{}

			stored, _ := runs.Get("run")
			if !reflect.DeepEqual(stored, original) {
				t.Fatalf("snapshot mutated tracker: original %#v, stored %#v", original, stored)
			}
		})
	}

	runs.Accept("accepted", RunTriggerScheduledJob, RunMetadata{})

	accepted, _ := runs.Get("accepted")
	if accepted.StartedAt != nil || accepted.FinishedAt != nil {
		t.Fatalf("snapshot fabricated timestamps: %#v", accepted)
	}
}

func TestScheduledRunLifecycleConcurrentRetries(t *testing.T) {
	t.Parallel()

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})
	runs.ScheduledRunStarted(scheduler.RunExecution{RunID: "run", JobName: "backup", Stack: "prod"})
	runs.ScheduledRunFinished("run", nil)
	terminal, _ := runs.Get("run")

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			runs.ScheduledRunStarted(scheduler.RunExecution{RunID: "run", JobName: "changed", Stack: "other"})
			runs.ScheduledRunFinished("run", errors.New("cleanup retry"))
			snapshot, _ := runs.Get("run")
			*snapshot.StartedAt = time.Time{}
			*snapshot.FinishedAt = time.Time{}
			snapshot.Deployments[0].Context = "mutated"
		})
	}

	wg.Wait()

	repeated, _ := runs.Get("run")
	if !reflect.DeepEqual(repeated, terminal) {
		t.Fatalf("concurrent retries changed terminal record: before %#v, after %#v", terminal, repeated)
	}
}
