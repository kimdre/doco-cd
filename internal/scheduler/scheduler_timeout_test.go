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

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/prometheus"
)

// newRestartTimeoutWorker returns a worker for a stopped restart-mode container.
// The container exits with code 0 after exitAfter. A zero exitAfter means the
// container runs until doco-cd stops it.
func newRestartTimeoutWorker(t *testing.T, exitAfter time.Duration) (*scheduler, *recordingRunReporter, *atomic.Int32) {
	t.Helper()

	var stops atomic.Int32

	worker, reporter := newRunTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/source/json"):
			_, _ = w.Write([]byte(`{"Id":"source","Config":{"StopTimeout":1},"State":{"Status":"exited","Running":false}}`))
		case strings.HasSuffix(r.URL.Path, "/containers/source/start"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/containers/source/wait"):
			// The daemon sends the headers at once, so the client can start the container.
			w.WriteHeader(http.StatusOK)

			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}

			var exit <-chan time.Time
			if exitAfter > 0 {
				exit = time.After(exitAfter)
			}

			select {
			case <-exit:
				_, _ = w.Write([]byte(`{"StatusCode":0}`))
			case <-r.Context().Done():
			}
		case strings.HasSuffix(r.URL.Path, "/containers/source/stop"):
			stops.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	return worker, reporter, &stops
}

func TestRunScheduledWorkloadWithTimeoutStopsRestartContainer(t *testing.T) {
	t.Parallel()

	worker, _, stops := newRestartTimeoutWorker(t, 0)
	cfg := docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeRestart, Timeout: 100 * time.Millisecond}
	now := time.Now()

	err := worker.runScheduledWorkloadWithTimeout(t.Context(), testRunJob(), cfg, nil, now, now)

	var timeoutErr *docker.JobTimeoutError
	if !errors.As(err, &timeoutErr) || timeoutErr.Timeout != cfg.Timeout {
		t.Fatalf("error = %v, want JobTimeoutError with timeout %s", err, cfg.Timeout)
	}

	if err.Error() != "exceeded timeout of 100ms" {
		t.Fatalf("error text = %q", err.Error())
	}

	if got := stops.Load(); got != 1 {
		t.Fatalf("container stop requests = %d, want 1", got)
	}
}

func TestRunScheduledWorkloadWithTimeoutFinishesInTime(t *testing.T) {
	t.Parallel()

	worker, _, stops := newRestartTimeoutWorker(t, 10*time.Millisecond)
	cfg := docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeRestart, Timeout: 5 * time.Second}
	now := time.Now()

	if err := worker.runScheduledWorkloadWithTimeout(t.Context(), testRunJob(), cfg, nil, now, now); err != nil {
		t.Fatalf("run error = %v", err)
	}

	if got := stops.Load(); got != 0 {
		t.Fatalf("container stop requests = %d, want 0", got)
	}
}

func TestRunScheduledWorkloadWithTimeoutIgnoresShutdown(t *testing.T) {
	t.Parallel()

	worker, _, stops := newRestartTimeoutWorker(t, 0)
	cfg := docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeRestart, Timeout: time.Minute}
	now := time.Now()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	err := worker.runScheduledWorkloadWithTimeout(ctx, testRunJob(), cfg, nil, now, now)
	if err == nil || errors.Is(err, docker.ErrScheduledJobTimeout) {
		t.Fatalf("run error = %v, want a non-timeout error", err)
	}

	if got := stops.Load(); got != 0 {
		t.Fatalf("container stop requests = %d, want 0", got)
	}
}

func TestStopTimedOutWorkloadRequiresRunIDForOneOff(t *testing.T) {
	t.Parallel()

	worker, _, stops := newRestartTimeoutWorker(t, 0)
	cfg := docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff, Timeout: time.Second}

	if err := worker.stopTimedOutWorkload(t.Context(), testRunJob(), cfg, nil); err == nil {
		t.Fatal("stop without run ID succeeded")
	}

	if got := stops.Load(); got != 0 {
		t.Fatalf("container stop requests = %d, want 0", got)
	}
}

func TestJobTimedOut(t *testing.T) {
	t.Parallel()

	expiredRunCtx := func(parent context.Context) context.Context {
		runCtx, cancel := context.WithTimeoutCause(parent, 0, &docker.JobTimeoutError{Timeout: time.Second})
		t.Cleanup(cancel)
		<-runCtx.Done()

		return runCtx
	}

	canceledParent, cancelParent := context.WithCancel(context.Background())
	cancelParent()

	activeRunCtx, cancelActive := context.WithCancel(context.Background())
	t.Cleanup(cancelActive)

	tests := []struct {
		name   string
		ctx    context.Context
		runCtx context.Context
		want   bool
	}{
		{name: "timeout takes precedence over workload result", ctx: context.Background(), runCtx: expiredRunCtx(context.Background()), want: true},
		{name: "workload finished in time", ctx: context.Background(), runCtx: activeRunCtx, want: false},
		{name: "parent canceled", ctx: canceledParent, runCtx: expiredRunCtx(canceledParent), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := jobTimedOut(tt.ctx, tt.runCtx); got != tt.want {
				t.Fatalf("jobTimedOut() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAutomaticTimeoutReleasesRunAndReportsFailure(t *testing.T) {
	t.Parallel()

	worker, reporter, stops := newRestartTimeoutWorker(t, 0)
	worker.wg = &sync.WaitGroup{}
	sender := &recordingSender{}
	worker.notifier = sender

	job := testRunJob()
	job.name = t.Name()
	job.labels[docker.DocoCDLabels.Deployment.Name] = t.Name()
	cfg := docker.JobScheduleConfig{
		ExecutionMode: docker.JobExecutionModeRestart,
		SkipRunning:   true,
		NotifyOn:      docker.JobNotifyFailure,
		Timeout:       100 * time.Millisecond,
	}
	labels := getScheduledRunMetricLabels(job, cfg, getJobStackName(job))
	errorsBefore := testutil.ToFloat64(prometheus.ScheduledRunErrorsTotal.WithLabelValues(labels...))
	skippedBefore := testutil.ToFloat64(prometheus.ScheduledRunSkippedTotal.WithLabelValues(append(labels, "still_running")...))

	worker.triggerRun(t.Context(), job, cfg, time.Now())
	worker.triggerRun(t.Context(), job, cfg, time.Now())
	worker.runs.Wait()

	if worker.isRunInProgress(job.key) {
		t.Fatal("timed-out run still holds skip_running")
	}

	worker.triggerRun(t.Context(), job, cfg, time.Now())
	worker.runs.Wait()

	if got := stops.Load(); got != 2 {
		t.Fatalf("container stop requests = %d, want two timed-out runs", got)
	}

	if len(reporter.starts) != 2 || len(reporter.finishes) != 2 || len(reporter.errors) != 2 {
		t.Fatalf("run lifecycle = %#v, want two completed runs", reporter)
	}

	for _, err := range reporter.errors {
		if !errors.Is(err, docker.ErrScheduledJobTimeout) {
			t.Fatalf("run error = %v, want job timeout", err)
		}
	}

	if len(sender.levels) != 2 || sender.levels[0] != notification.Failure || sender.levels[1] != notification.Failure {
		t.Fatalf("notification levels = %v, want two failures", sender.levels)
	}

	if got := testutil.ToFloat64(prometheus.ScheduledRunErrorsTotal.WithLabelValues(labels...)) - errorsBefore; got != 2 {
		t.Fatalf("scheduled run errors = %v, want 2", got)
	}

	if got := testutil.ToFloat64(prometheus.ScheduledRunsActive.WithLabelValues(labels...)); got != 0 {
		t.Fatalf("active scheduled runs = %v, want 0", got)
	}

	if got := testutil.ToFloat64(prometheus.ScheduledRunSkippedTotal.WithLabelValues(append(labels, "still_running")...)) - skippedBefore; got != 1 {
		t.Fatalf("skipped scheduled runs = %v, want only the overlapping run", got)
	}
}

func TestRequiresJobCompletion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  docker.JobScheduleConfig
		want bool
	}{
		{name: "no stop services and no timeout", cfg: docker.JobScheduleConfig{}, want: false},
		{name: "stop services", cfg: docker.JobScheduleConfig{StopServices: []docker.StopServiceRef{{Service: "db"}}}, want: true},
		{name: "timeout", cfg: docker.JobScheduleConfig{Timeout: time.Minute}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := requiresJobCompletion(tt.cfg); got != tt.want {
				t.Fatalf("requiresJobCompletion() = %v, want %v", got, tt.want)
			}
		})
	}
}

// recoveryTimeoutFake counts the Docker calls of a recovered one-off container.
type recoveryTimeoutFake struct {
	waits atomic.Int32
	stops atomic.Int32
}

// newRecoveryTimeoutWorker returns a worker with a recovered one-off container.
// The labels hold labelStartedAt and the job timeout. The container state holds
// stateStartedAt. A running container runs until doco-cd stops it. An exited
// container reports exitCode.
func newRecoveryTimeoutWorker(t *testing.T, labelStartedAt, stateStartedAt time.Time, timeout string, running bool, exitCode int) (*scheduler, *recordingRunReporter, *recoveryTimeoutFake) {
	t.Helper()

	fake := &recoveryTimeoutFake{}
	stopped := make(chan struct{})

	worker, reporter := newRunTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = w.Write([]byte(`[{"Id":"artifact"}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/artifact/json"):
			state := map[string]any{"Status": "exited", "Running": false, "ExitCode": exitCode, "StartedAt": stateStartedAt.Format(time.RFC3339Nano)}
			if running && fake.stops.Load() == 0 {
				state = map[string]any{"Status": "running", "Running": true, "StartedAt": stateStartedAt.Format(time.RFC3339Nano)}
			}

			if err := json.NewEncoder(w).Encode(map[string]any{
				"Id": "artifact",
				"Config": map[string]any{"StopTimeout": 1, "Labels": map[string]string{
					docker.DocoCDJobLabels.JobStartedAt: labelStartedAt.Format(time.RFC3339Nano),
					docker.DocoCDJobLabels.JobTimeout:   timeout,
				}},
				"State": state,
			}); err != nil {
				t.Error(err)
			}
		case strings.HasSuffix(r.URL.Path, "/containers/artifact/wait"):
			fake.waits.Add(1)

			// The daemon sends the headers at once.
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()

			select {
			case <-stopped:
				_, _ = w.Write([]byte(`{"StatusCode":143}`))
			case <-r.Context().Done():
			}
		case strings.HasSuffix(r.URL.Path, "/containers/artifact/stop"):
			if fake.stops.Add(1) == 1 {
				close(stopped)
			}

			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/containers/artifact"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	worker.executions = newExecutionStore(t.TempDir())

	return worker, reporter, fake
}

// recoverTimeoutRecord runs recovery for a new one-off execution record and returns the record.
func recoverTimeoutRecord(t *testing.T, worker *scheduler) *executionRecord {
	t.Helper()

	job := testRunJob()

	record := worker.newExecutionRecord("timeout-run", job, docker.JobScheduleConfig{ExecutionMode: docker.JobExecutionModeOneOff})
	if err := worker.executions.create(&record); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		worker.recoverExecution(t.Context(), &record, job)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("recovery did not finish")
	}

	return &record
}

func TestRecoveryStopsJobPastTimeout(t *testing.T) {
	t.Parallel()

	startedAt := time.Now().Add(-time.Hour).UTC()
	worker, reporter, fake := newRecoveryTimeoutWorker(t, startedAt, startedAt, "1m", true, 0)

	record := recoverTimeoutRecord(t, worker)

	if got := fake.stops.Load(); got != 1 {
		t.Fatalf("stop calls = %d, want 1", got)
	}

	if got := fake.waits.Load(); got != 0 {
		t.Fatalf("wait calls = %d, want 0 for an expired deadline", got)
	}

	if len(reporter.errors) != 1 || !errors.Is(reporter.errors[0], docker.ErrScheduledJobTimeout) {
		t.Fatalf("recovered run errors = %v, want job timeout", reporter.errors)
	}

	if !record.Reported {
		t.Fatal("timed out recovery did not finalize the record")
	}
}

func TestRecoveryEnforcesRemainingTimeout(t *testing.T) {
	t.Parallel()

	// The label time is far in the past. The timeout must count from the container start.
	labelStartedAt := time.Now().Add(-time.Hour).UTC()
	stateStartedAt := time.Now().Add(-4 * time.Second).UTC()
	worker, reporter, fake := newRecoveryTimeoutWorker(t, labelStartedAt, stateStartedAt, "5s", true, 0)

	begin := time.Now()
	record := recoverTimeoutRecord(t, worker)
	elapsed := time.Since(begin)

	if got := fake.waits.Load(); got != 1 {
		t.Fatalf("wait calls = %d, want 1", got)
	}

	if got := fake.stops.Load(); got != 1 {
		t.Fatalf("stop calls = %d, want 1", got)
	}

	// Recovery waits only for the remaining second, not for the full timeout.
	if elapsed < 500*time.Millisecond || elapsed >= 4*time.Second {
		t.Fatalf("recovery waited %s, want about 1s", elapsed)
	}

	if len(reporter.starts) != 1 || !reporter.starts[0].StartedAt.Equal(labelStartedAt) {
		t.Fatalf("recovered run start = %#v, want label start time", reporter.starts)
	}

	if len(reporter.errors) != 1 || !errors.Is(reporter.errors[0], docker.ErrScheduledJobTimeout) {
		t.Fatalf("recovered run errors = %v, want job timeout", reporter.errors)
	}

	if !record.Reported {
		t.Fatal("timed out recovery did not finalize the record")
	}
}

func TestRecoveryReportsFinishedJobPastTimeout(t *testing.T) {
	t.Parallel()

	startedAt := time.Now().Add(-time.Hour).UTC()
	worker, reporter, fake := newRecoveryTimeoutWorker(t, startedAt, startedAt, "1m", false, 0)

	recoverTimeoutRecord(t, worker)

	if got := fake.stops.Load(); got != 0 {
		t.Fatalf("stop calls = %d, want 0 for a finished job", got)
	}

	if len(reporter.errors) != 1 || reporter.errors[0] != nil {
		t.Fatalf("recovered run errors = %v, want the result of the finished job", reporter.errors)
	}
}

func TestRecoveryInvalidTimeoutKeepsHistory(t *testing.T) {
	t.Parallel()

	startedAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	worker, reporter, fake := newRecoveryTimeoutWorker(t, startedAt, startedAt, "nope", false, 7)

	recoverTimeoutRecord(t, worker)

	if got := fake.stops.Load(); got != 0 {
		t.Fatalf("stop calls = %d, want 0 for an invalid timeout", got)
	}

	if len(reporter.starts) != 1 || !reporter.starts[0].StartedAt.Equal(startedAt) {
		t.Fatalf("recovered run start = %#v, want label start time", reporter.starts)
	}

	var exitErr *docker.ContainerExitError
	if len(reporter.errors) != 1 || !errors.As(reporter.errors[0], &exitErr) || exitErr.ExitCode != 7 {
		t.Fatalf("recovered run errors = %v, want exit code 7", reporter.errors)
	}
}

func TestListJobsReportsTimeout(t *testing.T) {
	t.Parallel()

	worker, _ := newRunTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if !strings.HasSuffix(r.URL.Path, "/containers/json") {
			t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)

			return
		}

		if err := json.NewEncoder(w).Encode([]map[string]any{
			{
				"Id": "with-timeout", "Names": []string{"/backup"}, "State": "exited",
				"Labels": map[string]string{
					docker.DocoCDJobLabels.JobEnabled:   "true",
					docker.DocoCDJobLabels.JobSchedule:  "@every 1m",
					docker.DocoCDJobLabels.JobTimeout:   "90s",
					docker.DocoCDLabels.Deployment.Name: "stack",
				},
			},
			{
				"Id": "without-timeout", "Names": []string{"/report"}, "State": "exited",
				"Labels": map[string]string{
					docker.DocoCDJobLabels.JobEnabled:   "true",
					docker.DocoCDJobLabels.JobSchedule:  "@every 1m",
					docker.DocoCDLabels.Deployment.Name: "stack",
				},
			},
		}); err != nil {
			t.Error(err)
		}
	})

	jobs, err := worker.listJobs(t.Context(), "stack")
	if err != nil {
		t.Fatal(err)
	}

	timeouts := make(map[string]string, len(jobs))
	for _, job := range jobs {
		timeouts[job.Name] = job.Timeout
	}

	if timeouts["backup"] != "1m30s" || timeouts["report"] != "" {
		t.Fatalf("job timeouts = %v, want backup=1m30s and report empty", timeouts)
	}
}
