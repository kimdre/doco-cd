package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

type testScheduledJobOperations struct {
	listJobs   func(context.Context, string, string) ([]scheduler.JobInfo, error)
	triggerNow func(context.Context, string, string, string, *secretprovider.SecretProvider) (string, error)
}

func (f testScheduledJobOperations) ListJobs(ctx context.Context, contextName, stackName string) ([]scheduler.JobInfo, error) {
	if f.listJobs == nil {
		return []scheduler.JobInfo{}, nil
	}

	return f.listJobs(ctx, contextName, stackName)
}

func (f testScheduledJobOperations) TriggerNow(
	ctx context.Context,
	contextName string,
	jobName string,
	stackName string,
	secretProvider *secretprovider.SecretProvider,
) (string, error) {
	if f.triggerNow == nil {
		return "", nil
	}

	return f.triggerNow(ctx, contextName, jobName, stackName, secretProvider)
}

func TestNewRunsValidatesDependencies(t *testing.T) {
	valid := Dependencies{
		ScheduledJobs: testScheduledJobOperations{},
		Poll: PollDependencies{
			AppConfig: &app.Config{},
			Runner: func(
				context.Context,
				poll.Config,
				*app.Config,
				container.MountPoint,
				command.Cli,
				*docker.ContextRegistry,
				*slog.Logger,
				notification.Metadata,
				*secretprovider.SecretProvider,
				string,
			) error {
				return nil
			},
		},
	}

	for _, testCase := range []struct {
		name     string
		contains string
		mutate   func(*Dependencies)
	}{
		{name: "scheduled jobs", contains: "Dependencies.ScheduledJobs", mutate: func(deps *Dependencies) { deps.ScheduledJobs = nil }},
		{name: "poll app config", contains: "Dependencies.Poll.AppConfig", mutate: func(deps *Dependencies) { deps.Poll.AppConfig = nil }},
		{name: "poll runner", contains: "Dependencies.Poll.Runner", mutate: func(deps *Dependencies) { deps.Poll.Runner = nil }},
		{
			name:     "run limit",
			contains: "Dependencies.MaxRunsPerTrigger",
			mutate: func(deps *Dependencies) {
				deps.MaxRunsPerTrigger = map[RunTrigger]int{RunTriggerWebhook: 0}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dependencies := valid
			testCase.mutate(&dependencies)

			defer func() {
				recovered := recover()
				if recovered == nil || !strings.Contains(fmt.Sprint(recovered), testCase.contains) {
					t.Fatalf("NewRuns() panic = %v, want containing %q", recovered, testCase.contains)
				}
			}()

			NewRuns(t.Context(), slog.Default(), dependencies)
		})
	}
}

type testControlPlaneRunsOptions struct {
	applicationCtx context.Context
	background     *backgroundWork
	tracker        *deploymentRunTracker
	log            *logger.Logger
	scheduledJobs  ScheduledJobOperations
	appConfig      *app.Config
	dataMountPoint container.MountPoint
	dockerCli      command.Cli
	contexts       *docker.ContextRegistry
	secretProvider *secretprovider.SecretProvider
	pollRunner     PollRunner
}

func newTestControlPlaneRuns(t testing.TB, options testControlPlaneRunsOptions) *Runs {
	t.Helper()

	if options.applicationCtx == nil {
		options.applicationCtx = t.Context()
	}

	if options.background == nil {
		options.background = newBackgroundWork()
	}

	if options.tracker == nil {
		options.tracker = newDeploymentRunTracker(nil)
	}

	if options.log == nil {
		options.log = logger.New(logger.LevelCritical)
	}

	if options.appConfig == nil {
		options.appConfig = &app.Config{}
	}

	if options.scheduledJobs == nil {
		options.scheduledJobs = testScheduledJobOperations{}
	}

	if options.pollRunner == nil {
		options.pollRunner = func(
			context.Context,
			poll.Config,
			*app.Config,
			container.MountPoint,
			command.Cli,
			*docker.ContextRegistry,
			*slog.Logger,
			notification.Metadata,
			*secretprovider.SecretProvider,
			string,
		) error {
			return nil
		}
	}

	runs := newRuns(
		options.applicationCtx,
		options.background,
		options.tracker,
		options.log.Logger,
		newControlPlaneJobs(options.scheduledJobs, options.secretProvider),
		newControlPlanePoll(
			options.appConfig,
			options.dataMountPoint,
			options.dockerCli,
			options.contexts,
			options.secretProvider,
			options.pollRunner,
		),
	)
	t.Cleanup(runs.CloseAndWait)

	return runs
}

func TestControlPlaneRunsSynchronousLifecycle(t *testing.T) {
	t.Parallel()

	tracker := newDeploymentRunTracker(nil)
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{tracker: tracker})
	jobID := runs.Accept("sync", deploymentRunTriggerWebhook, RunMetadata{
		Repository: "owner/repo",
		Target:     "prod",
		Revision:   "main",
	})
	runs.AddDeployment(jobID, "api", "")

	if err := runs.Execute(t.Context(), jobID, RunExecution{
		Mode:         RunSynchronous,
		PanicContext: "test run",
		PanicError:   errors.New("test run panicked"),
	}, func(context.Context) (RunResult, error) {
		return SucceededRun("done"), nil
	}); err != nil {
		t.Fatal(err)
	}

	run, ok := runs.Get(jobID)
	if !ok || run.Status != deploymentRunStatusSucceeded || run.Message != "done" {
		t.Fatalf("run = %#v, found = %t", run, ok)
	}

	if run.Repository != "owner/repo" || run.Target != "prod" || run.Revision != "main" || len(run.Deployments) != 1 {
		t.Fatalf("run metadata = %#v", run)
	}
}

func TestControlPlaneRunsConcurrentAsynchronousRuns(t *testing.T) {
	t.Parallel()

	const count = 16

	tracker := newDeploymentRunTracker(nil)
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{tracker: tracker})
	started := make(chan struct{}, count)
	release := make(chan struct{})
	jobIDs := make([]string, 0, count)

	for i := range count {
		jobID := runs.Accept("concurrent-"+strconv.Itoa(i), deploymentRunTriggerPoll, RunMetadata{})

		jobIDs = append(jobIDs, jobID)
		if err := runs.Execute(t.Context(), jobID, RunExecution{
			Mode:         RunAsynchronous,
			PanicContext: "concurrent run",
			PanicError:   errors.New("concurrent run panicked"),
		}, func(context.Context) (RunResult, error) {
			started <- struct{}{}

			runs.AddDeployment(jobID, "stack", "context")

			return SucceededRun("done"), nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	for range count {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for concurrent runs")
		}
	}

	stopReading := make(chan struct{})
	readerDone := make(chan struct{})

	go func() {
		defer close(readerDone)

		for {
			select {
			case <-stopReading:
				return
			default:
				_ = runs.List(count, string(deploymentRunTriggerPoll), "")
				for _, jobID := range jobIDs {
					_, _ = runs.Get(jobID)
				}
			}
		}
	}()

	close(release)
	runs.background.Wait()
	close(stopReading)
	<-readerDone

	if got := runs.List(count, string(deploymentRunTriggerPoll), string(deploymentRunStatusSucceeded)); len(got) != count {
		t.Fatalf("succeeded runs = %d, want %d", len(got), count)
	}
}

func TestControlPlaneRunsConvertsPanic(t *testing.T) {
	t.Parallel()

	panicErr := errors.New("run panicked")
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})
	jobID := runs.Accept("panic", deploymentRunTriggerWebhook, RunMetadata{})

	err := runs.Execute(t.Context(), jobID, RunExecution{
		Mode:         RunSynchronous,
		PanicContext: "test run",
		PanicError:   panicErr,
	}, func(context.Context) (RunResult, error) {
		panic("sensitive panic value")
	})
	if !errors.Is(err, panicErr) {
		t.Fatalf("error = %v, want %v", err, panicErr)
	}

	run, _ := runs.Get(jobID)
	if run.Status != deploymentRunStatusFailed || run.Message != panicErr.Error() {
		t.Fatalf("run = %#v", run)
	}
}

func TestControlPlaneRunsRecordsSkippedOutcome(t *testing.T) {
	t.Parallel()

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})
	jobID := runs.Accept("skipped", deploymentRunTriggerWebhook, RunMetadata{})

	if err := runs.Execute(t.Context(), jobID, RunExecution{
		Mode:         RunSynchronous,
		PanicContext: "test run",
		PanicError:   errors.New("test run panicked"),
	}, func(context.Context) (RunResult, error) {
		return SkippedRun("not applicable"), nil
	}); err != nil {
		t.Fatal(err)
	}

	run, _ := runs.Get(jobID)
	if run.Status != deploymentRunStatusSkipped || run.Message != "not applicable" {
		t.Fatalf("run = %#v", run)
	}
}

func TestControlPlaneRunsSynchronousRequestCancellation(t *testing.T) {
	t.Parallel()

	requestCtx, cancelRequest := context.WithCancel(t.Context())
	cancelRequest()

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})
	jobID := runs.Accept("request-cancel", deploymentRunTriggerPoll, RunMetadata{})

	err := runs.Execute(requestCtx, jobID, RunExecution{
		Mode:         RunSynchronous,
		PanicContext: "test run",
		PanicError:   errors.New("test run panicked"),
	}, func(ctx context.Context) (RunResult, error) {
		return RunResult{}, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want request cancellation", err)
	}
}

func TestControlPlaneRunsDetachedSynchronousRequest(t *testing.T) {
	t.Parallel()

	requestCtx, cancelRequest := context.WithCancel(t.Context())
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})
	jobID := runs.Accept("detached-sync", deploymentRunTriggerWebhook, RunMetadata{})
	started := make(chan struct{})
	finish := make(chan struct{})
	result := make(chan error, 1)

	go func() {
		result <- runs.Execute(requestCtx, jobID, RunExecution{
			Mode:         RunSynchronousDetached,
			PanicContext: "test run",
			PanicError:   errors.New("test run panicked"),
		}, func(ctx context.Context) (RunResult, error) {
			close(started)
			<-finish

			return SucceededRun("done"), ctx.Err()
		})
	}()

	<-started
	cancelRequest()
	close(finish)

	if err := <-result; err != nil {
		t.Fatalf("detached synchronous run was cancelled with its request: %v", err)
	}
}

func TestControlPlaneRunsAsyncDetachesRequestAndCancelsOnShutdown(t *testing.T) {
	requestCtx, cancelRequest := context.WithCancel(t.Context())
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})
	jobID := runs.Accept("async", deploymentRunTriggerPoll, RunMetadata{})
	started := make(chan context.Context, 1)
	finished := make(chan error, 1)

	if err := runs.Execute(requestCtx, jobID, RunExecution{
		Mode:         RunAsynchronous,
		PanicContext: "test run",
		PanicError:   errors.New("test run panicked"),
	}, func(ctx context.Context) (RunResult, error) {
		started <- ctx

		<-ctx.Done()

		finished <- ctx.Err()

		return RunResult{}, ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}

	runCtx := <-started

	cancelRequest()

	select {
	case <-runCtx.Done():
		t.Fatal("async run was cancelled with its request")
	case <-time.After(20 * time.Millisecond):
	}

	runs.CloseAndWait()

	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown cancellation = %v", err)
	}
}

func TestControlPlaneRunsCloseCancelsAndDrains(t *testing.T) {
	t.Parallel()

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})
	jobID := runs.Accept("drain", deploymentRunTriggerScheduledJob, RunMetadata{})
	cancelled := make(chan struct{})
	release := make(chan struct{})

	if err := runs.Execute(t.Context(), jobID, RunExecution{
		Mode:         RunAsynchronous,
		PanicContext: "test run",
		PanicError:   errors.New("test run panicked"),
	}, func(ctx context.Context) (RunResult, error) {
		<-ctx.Done()
		close(cancelled)
		<-release

		return RunResult{}, ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}

	closed := make(chan struct{})

	go func() {
		runs.CloseAndWait()
		close(closed)
	}()

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the run")
	}

	select {
	case <-closed:
		t.Fatal("close returned before the run drained")
	default:
	}

	rejectedJobID := runs.Accept("rejected-during-close", deploymentRunTriggerPoll, RunMetadata{})

	err := runs.Execute(t.Context(), rejectedJobID, RunExecution{
		Mode:         RunSynchronous,
		PanicContext: "test run",
		PanicError:   errors.New("test run panicked"),
	}, func(context.Context) (RunResult, error) {
		t.Fatal("run started after shutdown began")

		return RunResult{}, nil
	})
	if !errors.Is(err, ErrBackgroundWorkClosed) {
		t.Fatalf("admission error = %v, want %v", err, ErrBackgroundWorkClosed)
	}

	close(release)

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close did not return after the run drained")
	}
}

func TestControlPlaneRunsRejectsAfterShutdownBegins(t *testing.T) {
	t.Parallel()

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})
	runs.CloseAndWait()

	jobID := runs.Accept("rejected", deploymentRunTriggerPoll, RunMetadata{})

	err := runs.Execute(t.Context(), jobID, RunExecution{
		Mode:         RunAsynchronous,
		PanicContext: "test run",
		PanicError:   errors.New("test run panicked"),
	}, func(context.Context) (RunResult, error) {
		t.Fatal("run started during shutdown")

		return RunResult{}, nil
	})
	if !errors.Is(err, ErrBackgroundWorkClosed) {
		t.Fatalf("error = %v, want %v", err, ErrBackgroundWorkClosed)
	}

	run, ok := runs.Get(jobID)
	if !ok || run.Status != deploymentRunStatusFailed || run.Message != ErrBackgroundWorkClosed.Error() {
		t.Fatalf("run = %#v, found = %t", run, ok)
	}
}

func waitForDeploymentRunStatus(t *testing.T, tracker *deploymentRunTracker, jobID string, want deploymentRunStatus) deploymentRun {
	t.Helper()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	for {
		run, ok := tracker.Get(jobID)
		if ok && run.Status == want {
			return run
		}

		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("timed out waiting for run %q status %q; last run: %#v", jobID, want, run)
		}
	}
}
