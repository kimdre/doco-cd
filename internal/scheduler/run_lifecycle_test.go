package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker"
)

type recordingRunReporter struct {
	mu       sync.Mutex
	starts   []RunExecution
	finishes []string
	errors   []error
}

func (r *recordingRunReporter) ScheduledRunStarted(run RunExecution) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.starts = append(r.starts, run)
}

func (r *recordingRunReporter) ScheduledRunFinished(runID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.finishes = append(r.finishes, runID)
	r.errors = append(r.errors, err)
}

type runTestCLI struct {
	command.Cli
	api client.APIClient
}

func (c runTestCLI) Client() client.APIClient { return c.api }

func newRunTestWorker(t *testing.T, handler http.HandlerFunc) (*scheduler, *recordingRunReporter) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	apiClient, err := client.New(client.WithHost(server.URL), client.WithAPIVersion("1.56"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = apiClient.Close() })

	reporter := &recordingRunReporter{}
	worker := newSchedulerForMode(docker.ContextClient{
		Name: "remote",
		Cli:  runTestCLI{api: apiClient},
	}, scheduledJobModeContainer, nil, nil, nil, nil, nil, nil, docker.ScheduledComposeOptions{})
	worker.runReporter = reporter

	return worker, reporter
}

func testRunJob() scheduledJob {
	return scheduledJob{
		key:     "remote::container:source",
		id:      "source",
		name:    "backup",
		mode:    scheduledJobModeContainer,
		context: "remote",
		labels: map[string]string{
			docker.DocoCDLabels.Deployment.Name: "stack",
		},
	}
}

func TestRuntimeLatestRunOrderingAndIsolation(t *testing.T) {
	t.Parallel()

	store := newRuntimeStore()
	now := time.Now()
	keys := []string{"container:source", "remote::container:source", "remote::swarm:source"}

	for _, key := range keys {
		store.setLatestRun(key, "old", now)
		store.setLatestRun(key, "new", now.Add(time.Second))
		store.setLatestRun(key, "recovered-old", now)
	}

	store.setLatestRun(keys[0], "new-z", now.Add(time.Second))
	store.setLatestRun(keys[0], "new-a", now.Add(time.Second))
	store.clearContextMode("remote", scheduledJobModeContainer)

	snapshot := store.latestRunsSnapshot()
	if snapshot[keys[0]].id != "new-z" || snapshot[keys[2]].id != "new" {
		t.Fatalf("unexpected latest runs: %#v", snapshot)
	}

	if _, exists := snapshot[keys[1]]; exists {
		t.Fatal("cleared context retained its latest run")
	}

	delete(snapshot, keys[0])

	if store.latestRunsSnapshot()[keys[0]].id != "new-z" {
		t.Fatal("snapshot mutation changed runtime state")
	}
}

func TestRuntimeLatestRunPruning(t *testing.T) {
	t.Parallel()

	store := newRuntimeStore()
	now := time.Now()
	job := testRunJob()
	store.setLatestRun(job.key, "disabled-source", now)
	store.setLatestRun("remote::container:removed", "removed", now)
	store.setLatestRun("remote::container:active", "active", now)
	store.setLatestRun("remote::container:new", "new", now.Add(time.Second))
	store.setLatestRun("remote::swarm:other", "other-mode", now)
	store.beginRun("remote", scheduledJobModeContainer, "remote::container:active")

	store.pruneLatestRuns("remote", scheduledJobModeContainer, []scheduledJob{job}, now)

	snapshot := store.latestRunsSnapshot()
	if len(snapshot) != 4 {
		t.Fatalf("latest runs after pruning = %#v", snapshot)
	}

	if snapshot[job.key].id != "disabled-source" {
		t.Fatal("pruning removed a discovered disabled source")
	}

	store.endRun("remote::container:active")
	store.pruneLatestRuns("remote", scheduledJobModeContainer, nil, now.Add(2*time.Second))

	if got := store.latestRunsSnapshot(); len(got) != 1 || got["remote::swarm:other"].id != "other-mode" {
		t.Fatalf("stale source associations were not cleared: %#v", got)
	}
}

func TestAutomaticRunReportsFailureAndSkipDoesNotReplace(t *testing.T) {
	t.Parallel()

	worker := newSchedulerForMode(docker.ContextClient{Name: "remote"}, scheduledJobModeContainer, nil, &sync.WaitGroup{}, nil, nil, nil, nil, docker.ScheduledComposeOptions{})
	reporter := &recordingRunReporter{}
	worker.runReporter = reporter
	job := testRunJob()
	job.mode = "unsupported"

	worker.triggerRun(t.Context(), job, docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeRestart}, time.Now())
	worker.runs.Wait()

	if len(reporter.starts) != 1 || len(reporter.finishes) != 1 || reporter.errors[0] == nil {
		t.Fatalf("unexpected run lifecycle: %#v", reporter)
	}

	run := reporter.starts[0]
	if run.RunID == "" || run.JobName != job.name || run.Context != "remote" || run.Stack != "stack" {
		t.Fatalf("incorrect execution metadata: %#v", run)
	}

	if reporter.finishes[0] != run.RunID || worker.runtime.latestRunsSnapshot()[job.key].id != run.RunID {
		t.Fatal("automatic execution used different run IDs")
	}

	job.running = true
	worker.triggerRun(t.Context(), job, docker.JobScheduleConfig{SkipRunning: true}, time.Now())
	worker.runs.Wait()

	if len(reporter.starts) != 1 || worker.runtime.latestRunsSnapshot()[job.key].id != run.RunID {
		t.Fatal("skipped schedule replaced the last execution")
	}
}

func TestFinishTrackedRunReportsPanic(t *testing.T) {
	t.Parallel()

	reporter := &recordingRunReporter{}
	worker := &scheduler{runReporter: reporter}

	func() {
		defer func() {
			if recovered := recover(); recovered != "test panic" {
				t.Fatalf("panic = %v, want test panic", recovered)
			}
		}()

		var runErr error
		defer func() {
			worker.finishTrackedRun("run", &runErr, recover())
		}()

		panic("test panic")
	}()

	if len(reporter.errors) != 1 || !errors.Is(reporter.errors[0], ErrScheduledRunPanicked) {
		t.Fatalf("panic was not recorded as failed: %#v", reporter.errors)
	}
}

func TestManualRunUsesProvidedIDAndListsDisabledHistory(t *testing.T) {
	t.Parallel()

	var enabled atomic.Bool
	enabled.Store(true)

	worker, reporter := newRunTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			if err := json.NewEncoder(w).Encode([]map[string]any{{
				"Id": "source", "Names": []string{"/backup"}, "State": "created",
				"Labels": map[string]string{
					docker.DocoCDJobLabels.JobEnabled:   map[bool]string{true: "true", false: "false"}[enabled.Load()],
					docker.DocoCDJobLabels.JobSchedule:  "@every 1m",
					docker.DocoCDLabels.Deployment.Name: "stack",
				},
			}}); err != nil {
				t.Error(err)
			}
		case strings.HasSuffix(r.URL.Path, "/containers/source/json"):
			_, _ = w.Write([]byte(`{"Id":"source","State":{"Status":"created","Running":false}}`))
		case strings.HasSuffix(r.URL.Path, "/containers/source/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	runID, err := worker.triggerNow(t.Context(), "manual-id", "backup", "stack")
	if err != nil || runID != "manual-id" {
		t.Fatalf("manual execution = %q, %v", runID, err)
	}

	if len(reporter.starts) != 1 || reporter.starts[0].RunID != runID || len(reporter.finishes) != 0 {
		t.Fatalf("manual tracking must leave finalization to the control plane: %#v", reporter)
	}

	enabled.Store(false)

	jobs, err := worker.listJobs(t.Context(), "stack")
	if err != nil || len(jobs) != 1 || jobs[0].LatestRunID != runID || jobs[0].Enabled {
		t.Fatalf("disabled history = %#v, %v", jobs, err)
	}

	_, err = worker.triggerNow(t.Context(), "rejected-id", "backup", "stack")
	if !errors.Is(err, ErrScheduledJobDisabled) || len(reporter.starts) != 1 {
		t.Fatalf("disabled trigger updated run history: %v, %#v", err, reporter)
	}
}

func TestRecoveredExecutionUsesOriginalIDAndTime(t *testing.T) {
	t.Parallel()

	originalStart := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	worker, reporter := newRunTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = w.Write([]byte(`[{"Id":"artifact","Names":["/artifact"]}]`))
		case strings.HasSuffix(r.URL.Path, "/containers/artifact/json"):
			if err := json.NewEncoder(w).Encode(map[string]any{
				"Id": "artifact",
				"Config": map[string]any{"Labels": map[string]string{
					docker.DocoCDJobLabels.JobStartedAt: originalStart.Format(time.RFC3339Nano),
				}},
				"State": map[string]any{"Status": "exited", "Running": false, "ExitCode": 0},
			}); err != nil {
				t.Error(err)
			}

		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/containers/artifact"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected recovery request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	job := testRunJob()
	record := worker.newExecutionRecord("recovered-id", job, docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff})
	worker.runtime.setLatestRun(job.key, "newer", originalStart.Add(time.Minute))

	worker.recoverExecution(context.Background(), &record, job)

	if len(reporter.starts) != 1 || !reporter.starts[0].StartedAt.Equal(originalStart) || reporter.starts[0].RunID != record.RunID {
		t.Fatalf("recovery lost original identity: %#v", reporter.starts)
	}

	if len(reporter.finishes) != 1 || reporter.errors[0] != nil || reporter.finishes[0] != record.RunID {
		t.Fatalf("recovery lifecycle = %#v", reporter)
	}

	if worker.runtime.latestRunsSnapshot()[job.key].id != "newer" {
		t.Fatal("recovered older execution replaced newer history")
	}

	worker.recoverExecution(t.Context(), &record, job)

	if len(reporter.starts) != 1 || len(reporter.finishes) != 1 {
		t.Fatal("reported cleanup retry fabricated another lifecycle")
	}
}

func TestAutomaticOneOffRunIdentityAndResult(t *testing.T) {
	t.Parallel()

	for _, exitCode := range []int{0, 7} {
		t.Run(map[int]string{0: "success", 7: "failure"}[exitCode], func(t *testing.T) {
			t.Parallel()

			started := make(chan struct{})
			release := make(chan struct{})
			labelsCreated := make(chan map[string]string, 1)
			worker, reporter := newRunTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")

				switch {
				case strings.HasSuffix(r.URL.Path, "/containers/source/json"):
					_, _ = w.Write([]byte(`{"Id":"source","Name":"/backup","Config":{"Image":"test"},"HostConfig":{}}`))
				case strings.HasSuffix(r.URL.Path, "/containers/create"):
					var request struct {
						Labels map[string]string `json:"Labels"`
					}

					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}

					labelsCreated <- request.Labels

					_, _ = w.Write([]byte(`{"Id":"artifact"}`))
				case strings.HasSuffix(r.URL.Path, "/containers/artifact/start"):
					close(started)
					w.WriteHeader(http.StatusNoContent)
				case strings.HasSuffix(r.URL.Path, "/containers/artifact/wait"):
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					<-release

					if err := json.NewEncoder(w).Encode(map[string]int{"StatusCode": exitCode}); err != nil {
						t.Error(err)
					}
				case strings.HasSuffix(r.URL.Path, "/containers/json"):
					_, _ = w.Write([]byte(`[{"Id":"artifact"}]`))
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/containers/artifact"):
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			worker.wg = &sync.WaitGroup{}
			worker.executions = newExecutionStore(t.TempDir())
			t.Cleanup(func() { worker.runs.Wait() })
			t.Cleanup(func() { close(release) })

			job := testRunJob()
			worker.triggerRun(t.Context(), job, docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff}, time.Now())

			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("one-off execution never started")
			}

			reporter.mu.Lock()
			run := reporter.starts[0]
			finishedCount := len(reporter.finishes)
			reporter.mu.Unlock()

			if finishedCount != 0 || worker.runtime.latestRunsSnapshot()[job.key].id != run.RunID {
				t.Fatal("active execution was not indexed before completion")
			}

			labels := <-labelsCreated
			if labels[docker.DocoCDJobLabels.JobRunID] != run.RunID ||
				labels[docker.DocoCDJobLabels.JobStartedAt] != run.StartedAt.Format(time.RFC3339Nano) {
				t.Fatalf("artifact identity differs from tracked run: %#v, %#v", labels, run)
			}

			records, err := worker.executions.list(worker.contextName, worker.mode)
			if err != nil || len(records) != 1 || records[0].RunID != run.RunID {
				t.Fatalf("retained execution ID = %#v, %v", records, err)
			}

			release <- struct{}{}

			worker.runs.Wait()

			if len(reporter.finishes) != 1 || reporter.finishes[0] != run.RunID ||
				(reporter.errors[0] != nil) != (exitCode != 0) {
				t.Fatalf("terminal lifecycle = %#v", reporter)
			}

			if got := worker.runtime.runStatusesSnapshot()[job.key]; got != formatExitStatus(exitCode) {
				t.Fatalf("one-off status = %q, want %q", got, formatExitStatus(exitCode))
			}
		})
	}
}

func TestAutomaticLaunchPersistenceFailureIsTracked(t *testing.T) {
	t.Parallel()

	worker := newSchedulerForMode(docker.ContextClient{Name: "remote"}, scheduledJobModeContainer, nil, &sync.WaitGroup{}, nil, nil, nil, nil, docker.ScheduledComposeOptions{})
	reporter := &recordingRunReporter{}
	worker.runReporter = reporter

	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	worker.executions = &executionStore{root: filepath.Join(blocked, "executions")}
	job := testRunJob()
	worker.triggerRun(t.Context(), job, docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff}, time.Now())
	worker.runs.Wait()

	if len(reporter.starts) != 1 || len(reporter.finishes) != 1 || reporter.errors[0] == nil ||
		!strings.Contains(reporter.errors[0].Error(), "persist scheduled execution before launch") {
		t.Fatalf("launch failure lifecycle = %#v", reporter)
	}
}
