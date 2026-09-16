package reconciliation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
)

func TestNewManagerAppliesDefaultDeploymentLimit(t *testing.T) {
	t.Parallel()

	manager := newTestManager(t)
	if got := cap(manager.limiter.sem); got != 1 {
		t.Fatalf("default deployment limit = %d, want 1", got)
	}
}

func TestNewManagerDoesNotApplyDefaultsToAppConfig(t *testing.T) {
	t.Parallel()

	appConfig := &app.Config{
		PollConfig: []poll.Config{{Interval: 0}},
	}
	newTestManagerWithDependencies(t, Dependencies{AppConfig: appConfig})

	if got := appConfig.PollConfig[0].Interval; got != 0 {
		t.Fatalf("poll interval = %s, want disabled interval", got)
	}
}

func TestNewManagerValidatesDependencies(t *testing.T) {
	t.Parallel()

	_, err := NewManager(Dependencies{})
	if err == nil || !strings.Contains(err.Error(), "validate reconciliation dependencies") {
		t.Fatalf("NewManager() error = %v, want dependency validation error", err)
	}
}

func TestManagerDeployValidatesRequest(t *testing.T) {
	t.Parallel()

	err := newTestManager(t).Deploy(t.Context(), DeployRequest{})
	if err == nil || !strings.Contains(err.Error(), "validate deploy request") {
		t.Fatalf("Deploy() error = %v, want request validation error", err)
	}
}

func TestManagerStateIsIsolated(t *testing.T) {
	t.Parallel()

	first := newTestManager(t)
	second := newTestManager(t)
	attrs := map[string]string{
		api.ProjectLabel: "proj",
		api.ServiceLabel: "db",
	}

	first.MarkSchedulerStopHeld("", "proj", "db")

	if second.schedulerHolds.isHeld("", attrs) {
		t.Fatal("expected reconciliation managers to own independent scheduler holds")
	}
}

func TestManagerCloseIsIdempotentAndRejectsDeployments(t *testing.T) {
	t.Parallel()

	manager := newTestManagerWithDependencies(t, Dependencies{})

	manager.Close()
	manager.Close()

	err := manager.Deploy(t.Context(), DeployRequest{})
	if !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Deploy() after Close error = %v, want %v", err, ErrManagerClosed)
	}
}

func TestManagerCloseCancelsJobsBeforeWaiting(t *testing.T) {
	t.Parallel()

	manager := newTestManagerWithDependencies(t, Dependencies{})

	jobCtx, cancel := context.WithCancel(context.Background())
	reconciliationJob := newJob(manager, DeployRequest{}, nil)
	reconciliationJob.cancel = cancel
	manager.jobs.jobs["repo"] = reconciliationJob
	manager.jobWG.Add(1)

	jobStopped := make(chan struct{})

	go func() {
		defer manager.jobWG.Done()

		<-jobCtx.Done()
		close(jobStopped)
	}()

	manager.Close()

	select {
	case <-jobStopped:
	default:
		t.Fatal("expected reconciliation job context to be cancelled before Close returned")
	}
}

// TestSchedulerStopHold_IsolatedAcrossContexts verifies that MarkSchedulerStopHeld/
// UnmarkSchedulerStopHeld/isServiceSchedulerStopHeld key holds by Docker context, so
// the same compose project/service name on two different Docker contexts are tracked
// independently and never suppress each other's reconciliation events.
func TestSchedulerStopHold_IsolatedAcrossContexts(t *testing.T) {
	r := newTestManager(t)

	attrs := map[string]string{
		api.ProjectLabel: "proj",
		api.ServiceLabel: "db",
	}

	r.MarkSchedulerStopHeld("", "proj", "db")

	if !r.schedulerHolds.isHeld("", attrs) {
		t.Fatal("expected service to be held stopped on the default context")
	}

	if r.schedulerHolds.isHeld("remote", attrs) {
		t.Fatal("expected hold on the default context to not be visible on the \"remote\" context")
	}

	r.MarkSchedulerStopHeld("remote", "proj", "db")

	if !r.schedulerHolds.isHeld("remote", attrs) {
		t.Fatal("expected service to be held stopped on the \"remote\" context")
	}

	// Releasing the default context's hold must not affect the remote context's hold.
	// (unmarkSchedulerStopHeld keeps a short grace-period entry alive after the
	// last release, so isServiceSchedulerStopHeld still reports true briefly.
	// See schedulerStopHoldGracePeriod, but this must remain scoped to the
	// default context and not leak into the remote context's independent hold.)
	r.UnmarkSchedulerStopHeld("", "proj", "db")

	if !r.schedulerHolds.isHeld("remote", attrs) {
		t.Fatal("expected remote context hold to remain held after releasing the unrelated default context hold")
	}
}

// TestSchedulerHeldServiceKey_DiffersByContext ensures the internal key builder used
// for the scheduler stop-hold map includes the Docker context, so same-named
// project/service pairs on different contexts never collide in the map.
func TestSchedulerHeldServiceKey_DiffersByContext(t *testing.T) {
	defaultKey := schedulerHeldServiceKey("", "proj", "db")
	explicitDefaultKey := schedulerHeldServiceKey("default", "proj", "db")
	remoteKey := schedulerHeldServiceKey("remote", "proj", "db")

	if defaultKey != explicitDefaultKey {
		t.Fatalf("schedulerHeldServiceKey() produced different keys for default context aliases: %q and %q", defaultKey, explicitDefaultKey)
	}

	if defaultKey == remoteKey {
		t.Fatalf("schedulerHeldServiceKey() produced the same key %q for different contexts", defaultKey)
	}
}

func TestStackDeploymentKey_NormalizesDefaultContext(t *testing.T) {
	defaultKey := stackDeploymentKey("repo", "", "stack")
	explicitDefaultKey := stackDeploymentKey("repo", "default", "stack")
	remoteKey := stackDeploymentKey("repo", "remote", "stack")

	if defaultKey != explicitDefaultKey {
		t.Fatalf("stackDeploymentKey() produced different keys for default context aliases: %q and %q", defaultKey, explicitDefaultKey)
	}

	if defaultKey == remoteKey {
		t.Fatalf("stackDeploymentKey() produced the same key %q for different contexts", defaultKey)
	}
}

// TestIsSchedulerStopHeld covers the exported query used by the pre-deploy stage
// (#1856) so a poll tick landing inside a scheduled job's stop window does not read
// doco-cd's own stop as drift.
func TestIsSchedulerStopHeld(t *testing.T) {
	r := newTestManager(t)

	if r.IsSchedulerStopHeld("", "proj", "db") {
		t.Fatal("expected no hold before the scheduler stopped anything")
	}

	r.MarkSchedulerStopHeld("", "proj", "db")

	if !r.IsSchedulerStopHeld("", "proj", "db") {
		t.Fatal("expected held service to be reported as held")
	}

	if r.IsSchedulerStopHeld("", "proj", "web") {
		t.Fatal("expected untouched service in the same project to not be held")
	}

	if r.IsSchedulerStopHeld("remote", "proj", "db") {
		t.Fatal("expected hold to be scoped to its docker context")
	}

	// The grace period keeps the hold alive shortly after the restart, which is what
	// covers a poll that starts while the service is still coming back up.
	r.UnmarkSchedulerStopHeld("", "proj", "db")

	if !r.IsSchedulerStopHeld("", "proj", "db") {
		t.Fatal("expected hold to stay active during the post-release grace period")
	}
}

// TestIsSchedulerStopHeldEmptyNames guards against empty project/service names
// matching an unrelated hold key.
func TestIsSchedulerStopHeldEmptyNames(t *testing.T) {
	r := newTestManager(t)

	r.MarkSchedulerStopHeld("", "proj", "db")

	if r.IsSchedulerStopHeld("", "", "") {
		t.Fatal("expected empty project/service to never be held")
	}

	if r.IsSchedulerStopHeld("", "proj", "") {
		t.Fatal("expected empty service name to never be held")
	}
}
