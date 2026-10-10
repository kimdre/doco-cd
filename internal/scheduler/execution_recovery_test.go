package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/docker"
)

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
