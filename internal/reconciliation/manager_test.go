package reconciliation

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/config/app"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestNewManagerAppliesDefaultDeploymentLimit(t *testing.T) {
	t.Parallel()

	manager := newTestManager(t)
	if got := cap(manager.limiter.sem); got != 1 {
		t.Fatalf("default deployment limit = %d, want 1", got)
	}

	if got := cap(manager.preDeployLimiter.sem); got != 1 {
		t.Fatalf("default pre-deployment limit = %d, want 1", got)
	}
}

func TestNewManagerUsesSeparatePreDeploymentLimit(t *testing.T) {
	t.Parallel()

	manager := newTestManagerWithDependencies(t, Dependencies{
		MaxConcurrentDeployments:    2,
		MaxConcurrentPreDeployments: 8,
	})

	if got := cap(manager.limiter.sem); got != 2 {
		t.Fatalf("deployment limit = %d, want 2", got)
	}

	if got := cap(manager.preDeployLimiter.sem); got != 8 {
		t.Fatalf("pre-deployment limit = %d, want 8", got)
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

func TestManagerAddJobKeepsJobOfNewerRevision(t *testing.T) {
	t.Parallel()

	repoDir, older, newer := newTestRepoWithTwoCommits(t)

	request := func(revision, reference string) DeployRequest {
		return DeployRequest{
			Logger:     slog.New(slog.DiscardHandler),
			JobTrigger: stages.JobTriggerWebhook,
			Repository: stages.RepositoryData{
				Name:              "repo",
				MirrorDir:         repoDir,
				Revision:          revision,
				ResolvedReference: reference,
			},
			DeployConfigs: []*deployConfig.Config{{Name: "web"}},
		}
	}

	manager := newTestManagerWithDependencies(t, Dependencies{})
	current := newJob(manager, request(newer, "refs/heads/main"), nil)
	manager.jobs.jobs["repo"] = current

	manager.addJob(t.Context(), request(older, "main"), nil)

	if manager.jobs.jobs["repo"] != current {
		t.Fatal("a request for an older revision replaced the reconciliation job")
	}

	select {
	case <-current.closeChan:
		t.Fatal("the reconciliation job of the newer revision was closed")
	default:
	}

	if !predatesJob(request(older, "main"), current.info) {
		t.Fatal("older revision of the same reference does not predate the job")
	}

	if predatesJob(request(newer, "main"), request(older, "main")) {
		t.Fatal("newer revision predates the job")
	}

	if predatesJob(request(older, "refs/tags/v1"), current.info) {
		t.Fatal("revision of another reference predates the job")
	}

	otherMirror := request(older, "main")
	otherMirror.Repository.MirrorDir = t.TempDir()

	if predatesJob(otherMirror, current.info) {
		t.Fatal("revision of another mirror predates the job")
	}
}

func TestWithWebhookFilteredConfigs(t *testing.T) {
	t.Parallel()

	main := &deployConfig.Config{Name: "main", WebhookEventFilter: "^refs/heads/main$"}
	dev := &deployConfig.Config{Name: "dev", WebhookEventFilter: "^refs/heads/dev$"}
	unfiltered := &deployConfig.Config{Name: "unfiltered"}
	deferred := &deployConfig.Config{Name: "deferred"}

	req := DeployRequest{
		JobTrigger:    stages.JobTriggerWebhook,
		Payload:       &webhook.ParsedPayload{Ref: "refs/heads/main"},
		DeployConfigs: []*deployConfig.Config{main, dev, unfiltered, deferred},
	}

	got := withWebhookFilteredConfigs(req, map[*deployConfig.Config]struct{}{deferred: {}})
	if len(got) != 2 {
		t.Fatalf("deferred configs = %d, want 2", len(got))
	}

	for _, dc := range []*deployConfig.Config{dev, deferred} {
		if _, ok := got[dc]; !ok {
			t.Fatalf("%s is not deferred", dc.Name)
		}
	}

	// Configs the filter skipped keep the deploy config of the previous job.
	previousDev := &deployConfig.Config{Name: "dev"}
	previous := newJob(nil, DeployRequest{DeployConfigs: []*deployConfig.Config{previousDev}}, nil)

	info, carried, _ := reconciliationJobInfo(req, got, previous)
	if _, ok := carried[previousDev]; !ok {
		t.Fatal("the deploy config skipped by the webhook event filter was not carried over")
	}

	for _, dc := range info.DeployConfigs {
		if dc == dev {
			t.Fatal("the deploy config skipped by the webhook event filter is reconciled")
		}
	}

	req.JobTrigger = stages.JobTriggerPoll
	if got := withWebhookFilteredConfigs(req, nil); got != nil {
		t.Fatalf("poll request deferred %d configs", len(got))
	}
}
