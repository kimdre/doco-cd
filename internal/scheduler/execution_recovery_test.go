package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/docker"
)

func TestRecoveryDoesNotAdoptActiveManualOneOffRun(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	recoveryInspected := make(chan struct{}, 1)

	var (
		releaseOnce       sync.Once
		artifactPresent   atomic.Bool
		artifactStartedAt atomic.Value
	)

	labels := map[string]string{
		docker.DocoCDLabels.Deployment.Name:     "stack",
		docker.DocoCDJobLabels.JobEnabled:       "true",
		docker.DocoCDJobLabels.JobSchedule:      "@every 24h",
		docker.DocoCDJobLabels.JobExecutionMode: "one_off",
	}
	manual, reporter := newRunTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			if strings.Contains(r.URL.Query().Get("filters"), docker.DocoCDJobLabels.JobRunID) {
				if artifactPresent.Load() {
					_, _ = w.Write([]byte(`[{"Id":"artifact"}]`))
				} else {
					_, _ = w.Write([]byte(`[]`))
				}

				return
			}

			if err := json.NewEncoder(w).Encode([]map[string]any{{
				"Id": "source", "Names": []string{"/backup"}, "State": "created", "Labels": labels,
			}}); err != nil {
				t.Error(err)
			}
		case strings.HasSuffix(r.URL.Path, "/containers/source/json"):
			_, _ = w.Write([]byte(`{"Id":"source","Name":"/backup","Config":{"Image":"test"},"HostConfig":{}}`))
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			var request struct {
				Labels map[string]string `json:"Labels"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}

			artifactStartedAt.Store(request.Labels[docker.DocoCDJobLabels.JobStartedAt])
			artifactPresent.Store(true)

			_, _ = w.Write([]byte(`{"Id":"artifact"}`))
		case strings.HasSuffix(r.URL.Path, "/containers/artifact/start"):
			close(started)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/containers/artifact/wait"):
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-release

			_, _ = w.Write([]byte(`{"StatusCode":0}`))
		case strings.HasSuffix(r.URL.Path, "/containers/artifact/json"):
			if err := json.NewEncoder(w).Encode(map[string]any{
				"Id": "artifact",
				"Config": map[string]any{"Labels": map[string]any{
					docker.DocoCDJobLabels.JobStartedAt: artifactStartedAt.Load(),
				}},
				"State": map[string]any{"Status": "running", "Running": true},
			}); err != nil {
				t.Error(err)
			}

			recoveryInspected <- struct{}{}
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/containers/artifact"):
			artifactPresent.Store(false)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	opts := docker.ScheduledComposeOptions{}
	opts.ComposeLoad.DataMountPath = t.TempDir()
	manual.executions = newExecutionStore(opts.ComposeLoad.DataMountPath)
	background := newSchedulerForMode(docker.ContextClient{
		Name: manual.contextName, Cli: manual.dockerCli,
	}, manual.mode, nil, nil, nil, nil, nil, manual.runtime, opts)

	job := testRunJob()
	nextRun := time.Now().Add(24 * time.Hour)
	manual.runtime.setStatesSnapshot(manual.contextName, manual.mode, map[string]scheduledJobState{
		job.key: {nextRun: nextRun},
	})

	var (
		runID  string
		runErr error
	)

	manualDone := make(chan struct{})
	go func() {
		defer close(manualDone)

		runID, runErr = manual.triggerNow(t.Context(), "", "backup", "stack")
	}()

	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		<-manualDone
		background.runs.Wait()
	})

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("manual execution did not start")
	}

	background.recoverExecutions(t.Context())

	recoveryDone := make(chan struct{})

	go func() {
		background.runs.Wait()
		close(recoveryDone)
	}()

	select {
	case <-recoveryInspected:
		t.Error("background recovery adopted a live manual execution")
	case <-recoveryDone:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery neither skipped nor adopted the manual execution")
	}

	releaseOnce.Do(func() { close(release) })
	<-manualDone
	<-recoveryDone

	if runErr != nil {
		t.Fatalf("manual run failed: %v", runErr)
	}

	if len(reporter.starts) != 1 || len(reporter.finishes) != 1 || reporter.errors[0] != nil {
		t.Errorf("manual run was finalized more than once: %#v", reporter)
	}

	jobs, err := manual.listJobs(t.Context(), "stack")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list jobs: %#v, %v", jobs, err)
	}

	if jobs[0].Status != "exited (0)" || jobs[0].LatestRunID != runID || jobs[0].LastRunAt == nil {
		t.Errorf("completed manual job = %#v, want its successful execution result", jobs[0])
	}

	if jobs[0].NextRunAt == nil || !jobs[0].NextRunAt.Equal(nextRun) {
		t.Errorf("manual execution changed next run: %v, want %v", jobs[0].NextRunAt, nextRun)
	}
}

func TestRecoveryClaimsSharedAcrossWorkers(t *testing.T) {
	t.Parallel()

	runtime := newRuntimeStore()
	manual := &scheduler{runtime: runtime}
	background := &scheduler{runtime: runtime}
	independent := &scheduler{runtime: newRuntimeStore()}

	if !manual.claimRecovery("run") {
		t.Fatal("manual worker could not claim its run")
	}

	if background.claimRecovery("run") {
		t.Fatal("background worker claimed a live manual run")
	}

	if !independent.claimRecovery("run") {
		t.Fatal("another Manager inherited the execution claim")
	}

	manual.releaseRecovery("run")

	if !background.claimRecovery("run") {
		t.Fatal("released execution could not be recovered")
	}

	background.releaseRecovery("run")
	independent.releaseRecovery("run")
}

func TestRecoverySkipsRemovedExecutionRecord(t *testing.T) {
	t.Parallel()

	worker, reporter := newRunTestWorker(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("removed execution was recovered: %s %s", r.Method, r.URL)
	})
	worker.executions = newExecutionStore(t.TempDir())
	job := testRunJob()

	record := worker.newExecutionRecord("completed-run", job, docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff})
	if err := worker.executions.create(&record); err != nil {
		t.Fatal(err)
	}

	candidates, err := worker.executions.list(worker.contextName, worker.mode)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("recovery candidates = %#v, %v", candidates, err)
	}

	worker.runtime.setRunStatus(job.key, "exited (0)")
	worker.runtime.setLatestRun(job.key, record.RunID, time.Now())

	if err := worker.executions.remove(record.RunID); err != nil {
		t.Fatal(err)
	}

	worker.recoverExecutionCandidate(t.Context(), candidates[0])
	worker.runs.Wait()

	if got := worker.runtime.runStatusesSnapshot()[job.key]; got != "exited (0)" {
		t.Fatalf("stale recovery cleared successful status: %q", got)
	}

	if len(reporter.starts) != 0 || len(reporter.finishes) != 0 || worker.runtime.isRunInProgress(job.key) {
		t.Fatal("stale recovery started another execution lifecycle")
	}

	if !worker.claimRecovery(record.RunID) {
		t.Fatal("skipping a removed record leaked its claim")
	}

	worker.releaseRecovery(record.RunID)
}

func TestRecoveryUsesCurrentReportingState(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32

	worker, reporter := newRunTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)

		if !strings.HasSuffix(r.URL.Path, "/containers/json") {
			t.Errorf("reported execution was monitored again: %s %s", r.Method, r.URL)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	worker.executions = newExecutionStore(t.TempDir())
	job := testRunJob()

	candidate := worker.newExecutionRecord("reported-run", job, docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff})
	if err := worker.executions.create(&candidate); err != nil {
		t.Fatal(err)
	}

	current := candidate
	current.Reported = true

	current.Restored = true
	if err := worker.executions.update(current); err != nil {
		t.Fatal(err)
	}

	worker.runtime.setRunStatus(job.key, "exited (0)")
	worker.recoverExecutionCandidate(t.Context(), candidate)
	worker.runs.Wait()

	if got := worker.runtime.runStatusesSnapshot()[job.key]; got != "exited (0)" {
		t.Fatalf("reported cleanup retry changed successful status: %q", got)
	}

	if len(reporter.starts) != 0 || len(reporter.finishes) != 0 || requests.Load() != 1 {
		t.Fatalf("reported cleanup repeated monitoring: %d requests, %#v", requests.Load(), reporter)
	}

	record, err := worker.executions.get(candidate.RunID)
	if err != nil || record != nil {
		t.Fatalf("reported execution record not removed: %#v, %v", record, err)
	}
}

func TestRecoveryAdoptsUnclaimedExecution(t *testing.T) {
	t.Parallel()

	for _, exitCode := range []int{0, 7} {
		t.Run(map[int]string{0: "success", 7: "failure"}[exitCode], func(t *testing.T) {
			t.Parallel()

			originalStart := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			worker, reporter := newRunTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")

				switch {
				case strings.HasSuffix(r.URL.Path, "/containers/json"):
					_, _ = w.Write([]byte(`[{"Id":"artifact"}]`))
				case strings.HasSuffix(r.URL.Path, "/containers/artifact/json"):
					if err := json.NewEncoder(w).Encode(map[string]any{
						"Id": "artifact",
						"Config": map[string]any{"Labels": map[string]string{
							docker.DocoCDJobLabels.JobStartedAt: originalStart.Format(time.RFC3339Nano),
						}},
						"State": map[string]any{"Status": "exited", "Running": false, "ExitCode": exitCode},
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
			worker.executions = newExecutionStore(t.TempDir())
			job := testRunJob()

			record := worker.newExecutionRecord("orphan-run", job, docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff})
			if err := worker.executions.create(&record); err != nil {
				t.Fatal(err)
			}

			worker.recoverExecutions(t.Context())
			worker.runs.Wait()

			if len(reporter.starts) != 1 || !reporter.starts[0].StartedAt.Equal(originalStart) ||
				len(reporter.finishes) != 1 || (reporter.errors[0] != nil) != (exitCode != 0) {
				t.Fatalf("orphan recovery lifecycle = %#v", reporter)
			}

			if got := worker.runtime.runStatusesSnapshot()[job.key]; got != formatExitStatus(exitCode) {
				t.Fatalf("orphan result = %q, want %q", got, formatExitStatus(exitCode))
			}

			if !worker.claimRecovery(record.RunID) {
				t.Fatal("orphan recovery leaked its execution claim")
			}

			worker.releaseRecovery(record.RunID)
		})
	}
}

func TestRecoveredRunWithoutOriginalTimeDoesNotReplaceHistory(t *testing.T) {
	t.Parallel()

	worker, reporter := newRunTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/containers/json") {
			t.Errorf("unexpected Docker request: %s", r.URL)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})
	job := testRunJob()
	worker.runtime.setLatestRun(job.key, "known-run", time.Now())
	record := worker.newExecutionRecord("missing-artifact", job, docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff})

	worker.recoverExecution(t.Context(), &record, job)

	if len(reporter.starts) != 0 || len(reporter.finishes) != 0 {
		t.Fatal("recovery without original timestamps fabricated history")
	}

	if got := worker.runtime.latestRunsSnapshot()[job.key].id; got != "known-run" {
		t.Fatalf("latest run = %q, want known-run", got)
	}
}

func TestRecoveredRunStartTimeLookupErrorStillFinalizes(t *testing.T) {
	t.Parallel()

	var listRequests atomic.Int32

	worker, reporter := newRunTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/containers/json") {
			t.Errorf("unexpected Docker request: %s", r.URL)
		}

		w.Header().Set("Content-Type", "application/json")

		if listRequests.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"temporary failure"}`))

			return
		}

		_, _ = w.Write([]byte("[]"))
	})
	job := testRunJob()
	record := worker.newExecutionRecord("lookup-error", job, docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff})

	worker.recoverExecution(t.Context(), &record, job)

	if !record.Reported {
		t.Fatal("start-time lookup error stopped recovery finalization")
	}

	if len(reporter.starts) != 0 || len(reporter.finishes) != 0 {
		t.Fatal("recovery without original timestamps fabricated history")
	}
}

func TestRecoveredRunCancellationRetainsArtifact(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	worker, reporter := newRunTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = w.Write([]byte(`[{"Id":"artifact"}]`))
		case strings.HasSuffix(r.URL.Path, "/containers/artifact/json"):
			if err := json.NewEncoder(w).Encode(map[string]any{
				"Id": "artifact",
				"Config": map[string]any{"Labels": map[string]string{
					docker.DocoCDJobLabels.JobStartedAt: time.Now().UTC().Format(time.RFC3339Nano),
				}},
				"State": map[string]any{"Status": "running", "Running": true},
			}); err != nil {
				t.Error(err)
			}
		case strings.HasSuffix(r.URL.Path, "/containers/artifact/wait"):
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
		default:
			t.Errorf("cancelled recovery changed its artifact: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	job := testRunJob()
	record := worker.newExecutionRecord("cancelled-run", job, docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)

		worker.recoverExecution(ctx, &record, job)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not start monitoring")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled recovery did not stop")
	}

	if record.Reported || record.Restored {
		t.Fatal("cancelled recovery changed finalization state")
	}

	if len(reporter.starts) != 1 || len(reporter.finishes) != 1 || !errors.Is(reporter.errors[0], context.Canceled) {
		t.Fatalf("cancelled recovery lifecycle = %#v", reporter)
	}
}
