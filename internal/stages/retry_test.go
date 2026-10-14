package stages

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/common/types/duration"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/notification"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
	"github.com/kimdre/doco-cd/internal/selfupdate"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

func newRetryTestManager(t *testing.T, stack string) *StageManager {
	t.Helper()
	manager := newTestStageManager(t)
	manager.DeployConfig = deploy.New(stack, "main")
	manager.DeployConfig.Retry.Limit = new(0)
	manager.Repository.Revision = "actual-application-revision"
	manager.Repository.ConfigRevision = "different-config-revision"
	manager.Repository.ConfigSourceUrl = "https://config.example.com/owner/config.git"
	manager.Docker.Project = &types.Project{
		Name:     stack,
		Services: types.Services{"web": {Name: "web", Image: "test:latest"}},
	}
	manager.Docker.ProjectHash = "normalized-project"
	t.Cleanup(manager.clearDeploymentFailure)

	return manager
}

func recordRetryTestFailure(t *testing.T, sm *StageManager, stage StageName) {
	t.Helper()

	if err := sm.prepareDeploymentRetry(t.Context(), sm.Log); err != nil {
		t.Fatal(err)
	}

	sm.retryAttempt.Start()
	sm.recordDeploymentFailure(stage, errors.New("broken"))
}

func TestRetrySuppressedErrorContractAndMerge(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	delay := &RetrySuppressedError{Stacks: []RetrySuppressedStack{{
		Name: "web", Context: "remote", Revision: "sha256:broken", Reason: "delay",
		NextRetry: deadline, FailedStage: "deploy", FailureReason: "bad hook", RetriesUsed: 1, RetryLimit: new(2),
	}}}
	exhausted := &RetrySuppressedError{Stacks: []RetrySuppressedStack{{
		Name: "db", Reason: "budget_exhausted", RetriesUsed: 2, RetryLimit: new(2),
	}}}

	merged := MergeRetrySuppressed([]*RetrySuppressedError{delay, nil, exhausted})
	if !errors.Is(merged, ErrRetrySuppressed) || !errors.Is(merged, ErrSkipDeployment) {
		t.Fatal("retry suppression must remain distinct and participate in skip flow")
	}

	if len(merged.Stacks) != 2 || merged.Stacks[0].Name != "db" || !merged.Stacks[1].NextRetry.Equal(deadline) {
		t.Fatalf("merge lost per-target information: %+v", merged)
	}

	if !strings.Contains(merged.Error(), "budget_exhausted") || !strings.Contains(merged.Error(), deadline.Format(time.RFC3339)) {
		t.Fatalf("unexpected suppression description: %s", merged)
	}

	if MergeRetrySuppressed(nil) != nil || MergeRetrySuppressed([]*RetrySuppressedError{nil}) != nil {
		t.Fatal("empty errors must merge to nil")
	}
}

func TestRetryGateUsesResolvedSourceAndIgnoresSyntheticForceRecreate(t *testing.T) {
	t.Parallel()
	sm := newRetryTestManager(t, "retry-source-identity")
	recordRetryTestFailure(t, sm, StageDeploy)

	failure, _ := sm.lastDeploymentFailure()
	if failure.CommitSHA != sm.Repository.Revision || failure.CommitSHA == sm.Repository.ConfigRevision {
		t.Fatalf("stored triggering/config revision instead of actual application revision: %+v", failure)
	}

	sm.DeployConfig.ForceRecreate = true
	sm.Origin = syncwindow.OriginReconciliation
	err := sm.prepareDeploymentRetry(t.Context(), sm.Log)

	var suppressed *RetrySuppressedError
	if !errors.As(err, &suppressed) || suppressed.Stacks[0].Reason != "budget_exhausted" {
		t.Fatalf("automatic force recreate bypassed exhausted budget: %v", err)
	}

	sm.Repository.ConfigRevision = "new-config-commit-with-identical-config"
	if err := sm.prepareDeploymentRetry(t.Context(), sm.Log); !errors.Is(err, ErrRetrySuppressed) {
		t.Fatal("configuration source revision alone reset budget")
	}

	sm.Repository.Revision = "fixed-application-revision"
	if err := sm.prepareDeploymentRetry(t.Context(), sm.Log); err != nil {
		t.Fatalf("actual application revision did not reset budget: %v", err)
	}
}

func TestRetryGateConfigAndProjectChangesResetBudget(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(*StageManager)
	}{
		{name: "config", change: func(sm *StageManager) { sm.DeployConfig.Timeout = duration.Duration(time.Hour) }},
		{name: "project", change: func(sm *StageManager) { sm.Docker.ProjectHash = "fixed-project" }},
		{name: "desired image", change: func(sm *StageManager) {
			sm.retryDesired.ImageDigests = map[string]string{"web": "sha256:fixed"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sm := newRetryTestManager(t, "retry-change-"+tc.name)
			sm.retryDesired = &docker.DeploymentDesiredState{ImageDigests: map[string]string{"web": "sha256:broken"}}
			recordRetryTestFailure(t, sm, StageCleanup)
			tc.change(sm)

			if err := sm.prepareDeploymentRetry(t.Context(), sm.Log); err != nil {
				t.Fatalf("desired state change did not reset budget: %v", err)
			}
		})
	}
}

func TestRetryGateManualFailurePreservesPauseAndSuccessClears(t *testing.T) {
	t.Parallel()
	sm := newRetryTestManager(t, "retry-manual-lifecycle")
	recordRetryTestFailure(t, sm, StageDeploy)

	sm.Origin = syncwindow.OriginManual
	if err := sm.prepareDeploymentRetry(t.Context(), sm.Log); err != nil {
		t.Fatalf("manual limit 0 override was suppressed: %v", err)
	}

	sm.retryAttempt.Start()
	sm.recordDeploymentFailure(StageCleanup, errors.New("manual failed differently"))

	failure, _ := sm.lastDeploymentFailure()
	if failure.RetriesUsed != 0 || failure.Stage != string(StageCleanup) || failure.Error != "manual failed differently" {
		t.Fatalf("manual failure lost pause/details: %+v", failure)
	}

	sm.Origin = ""
	if err := sm.prepareDeploymentRetry(t.Context(), sm.Log); !errors.Is(err, ErrRetrySuppressed) {
		t.Fatalf("zero origin must default automatic and remain paused: %v", err)
	}

	sm.Origin = syncwindow.OriginManual
	if err := sm.prepareDeploymentRetry(t.Context(), sm.Log); err != nil {
		t.Fatal(err)
	}

	order := retryStageOrder(sm, nil)
	if err := sm.runStageOrder(t.Context(), nil, order); err != nil {
		t.Fatal(err)
	}

	if _, failed := sm.lastDeploymentFailure(); failed {
		t.Fatal("successful manual deploy did not clear recovery marker and budget")
	}
}

func TestRetryFailedManualBranchAliasDoesNotRefillBudget(t *testing.T) {
	t.Parallel()
	sm := newRetryTestManager(t, "retry-manual-reference-alias")
	sm.Repository.Source = config.SourceTypeGit
	sm.DeployConfig.Reference = "refs/heads/main"
	sm.DeployConfig.Retry.Limit = new(1)
	recordRetryTestFailure(t, sm, StageDeploy)
	recordRetryTestFailure(t, sm, StageDeploy)

	before, _ := sm.lastDeploymentFailure()
	if before.RetriesUsed != 1 {
		t.Fatalf("expected exhausted single-retry budget: %+v", before)
	}

	sm.Origin = syncwindow.OriginManual

	sm.DeployConfig.Reference = "main"
	if err := sm.prepareDeploymentRetry(t.Context(), sm.Log); err != nil {
		t.Fatalf("manual alias override: %v", err)
	}

	sm.retryAttempt.Start()
	sm.recordDeploymentFailure(StageDeploy, errors.New("manual failed differently"))

	after, _ := sm.lastDeploymentFailure()
	if after.RetriesUsed != before.RetriesUsed || after.CommitSHA != before.CommitSHA {
		t.Fatalf("equivalent branch spelling refilled budget: before=%+v after=%+v", before, after)
	}

	sm.Origin = syncwindow.OriginAutomatic
	sm.DeployConfig.Reference = "refs/heads/main"

	var suppressed *RetrySuppressedError

	err := sm.prepareDeploymentRetry(t.Context(), sm.Log)
	if !errors.As(err, &suppressed) || suppressed.Stacks[0].Reason != "budget_exhausted" || suppressed.Stacks[0].RetriesUsed != 1 {
		t.Fatalf("automatic branch spelling rearmed exhausted release: %v", err)
	}
}

func TestRetryBranchAliasPreservesManualFailureDelay(t *testing.T) {
	t.Parallel()
	sm := newRetryTestManager(t, "retry-manual-alias-delay")
	sm.Repository.Source = config.SourceTypeGit
	sm.DeployConfig.Reference = "refs/heads/main"
	sm.DeployConfig.Retry.Limit = new(2)
	sm.DeployConfig.Retry.Delay = duration.Duration(time.Hour)
	recordRetryTestFailure(t, sm, StageDeploy)
	sm.Origin = syncwindow.OriginManual
	sm.DeployConfig.Reference = "main"
	recordRetryTestFailure(t, sm, StageDeploy)
	failure, _ := sm.lastDeploymentFailure()
	sm.Origin = syncwindow.OriginAutomatic
	sm.DeployConfig.Reference = "refs/heads/main"

	var suppressed *RetrySuppressedError

	err := sm.prepareDeploymentRetry(t.Context(), sm.Log)
	if !errors.As(err, &suppressed) || suppressed.Stacks[0].Reason != "delay" ||
		!suppressed.Stacks[0].NextRetry.Equal(failure.FailedAt.Add(time.Hour)) {
		t.Fatalf("equivalent alias lost manual actual-failure delay: %v", err)
	}
}

func TestRetryReferenceIdentityPreservesDistinctBranchesAndTags(t *testing.T) {
	t.Parallel()
	sm := newRetryTestManager(t, "retry-reference-normalization")
	sm.Repository.Source = config.SourceTypeGit

	base, err := sm.deploymentDesiredState()
	if err != nil {
		t.Fatal(err)
	}

	sm.DeployConfig.Reference = " refs/heads/main "

	alias, err := sm.deploymentDesiredState()
	if err != nil {
		t.Fatal(err)
	}

	if alias.ConfigHash != base.ConfigHash {
		t.Fatal("equivalent branch spelling changed effective identity")
	}

	for _, reference := range []string{"refs/heads/other", "refs/tags/main"} {
		sm.DeployConfig.Reference = reference

		distinct, err := sm.deploymentDesiredState()
		if err != nil {
			t.Fatal(err)
		}

		if distinct.ConfigHash == base.ConfigHash {
			t.Fatalf("different branch/tag %q collapsed to main branch", reference)
		}
	}
}

func TestRetryIdentityNormalizesPKICertificatesAndRuntimeValues(t *testing.T) {
	t.Parallel()
	sm := newRetryTestManager(t, "retry-pki-identity")
	sm.DeployConfig.ExternalSecrets = map[string]secrettypes.ExternalSecretRef{"CERT": {LegacyRef: "pki-role:issuer:role:app.example"}}
	identity := func(cert string) docker.DeploymentDesiredState {
		sm.DeployConfig.Internal.Environment = map[string]string{"CERT": cert}
		sm.DeployConfig.Internal.Hash = cert
		sm.DeployConfig.Internal.ConfigSourceWorkingDir = cert
		sm.Docker.Project.Services["web"] = types.ServiceConfig{
			Name: "web", Image: "test:latest", Environment: types.MappingWithEquals{"CERT": &cert},
		}

		hash, err := docker.ProjectHash(docker.WithNormalizedEnvValues(sm.Docker.Project,
			pkiRoleNormMap(sm.DeployConfig.ExternalSecrets, sm.DeployConfig.Internal.Environment)), "")
		if err != nil {
			t.Fatal(err)
		}

		sm.Docker.ProjectHash = hash

		desired, err := sm.deploymentDesiredState()
		if err != nil {
			t.Fatal(err)
		}

		return desired
	}
	first := identity("first-certificate")
	sm.JobID = "different-job"
	sm.DeployState.changedServices = []docker.Change{{Type: "runtime drift"}}
	sm.DeployConfig.ForceRecreate = true

	second := identity("rotated-certificate")
	if first.ConfigHash != second.ConfigHash || first.ProjectHash != second.ProjectHash {
		t.Fatalf("rotating PKI/runtime values changed identity: %+v %+v", first, second)
	}
}

func TestRetryFailureTargetIsolation(t *testing.T) {
	t.Parallel()
	sm := newRetryTestManager(t, "retry-target-isolation")
	recordRetryTestFailure(t, sm, StageDeploy)
	same := sm.deploymentFailureTarget()

	sm.DeployConfig.Context = " DEFAULT "
	if sm.deploymentFailureTarget().Context == same.Context {
		t.Fatal("test must exercise context normalization in the store")
	}

	if err := sm.prepareDeploymentRetry(t.Context(), sm.Log); !errors.Is(err, ErrRetrySuppressed) {
		t.Fatal("default context spellings did not share budget")
	}

	for _, mutate := range []func(){
		func() { sm.DeployConfig.Context = "remote" },
		func() { sm.DeployConfig.Internal.ConfigTarget = "other" },
		func() { sm.Repository.ConfigSourceUrl = "https://other.example/config.git" },
	} {
		mutate()

		if err := sm.prepareDeploymentRetry(t.Context(), sm.Log); err != nil {
			t.Fatalf("different target shared budget: %v", err)
		}
	}

	docker.DeploymentFailures.Clear(same)
}

func TestRetrySourceIdentityDoesNotRetainCredentials(t *testing.T) {
	t.Parallel()

	a := retrySourceIdentity("https://user:first@example.com/owner/config.git?token=first")

	b := retrySourceIdentity("https://user:second@example.com/owner/config.git?token=second")
	if a != b || strings.Contains(a, "user") || strings.Contains(a, "example.com") {
		t.Fatalf("credential rotation changed/leaked configuration source identity: %q %q", a, b)
	}

	if retrySourceIdentity("ghcr.io/owner/config:old") != retrySourceIdentity("ghcr.io/owner/config:fixed") {
		t.Fatal("OCI config source reference created a different target key")
	}

	if retrySourceIdentity("git@example.com:owner/config.git") != retrySourceIdentity("https://example.com/owner/config.git") {
		t.Fatal("equivalent Git source identities created different target keys")
	}
}

// These functions exercise the real stage runner while replacing only Docker
// stage work, keeping admission, notifications, failure recording and clearing.
func retryStageOrder(sm *StageManager, deployStage func(context.Context) error) StageOrder {
	stub := func(context.Context, *slog.Logger) error { return nil }

	return StageOrder{
		Order: []StageName{StageInit, StagePreDeploy, StageDeploy, StagePostDeploy, StageCleanup},
		Funcs: map[StageName]StageFunc{
			StageInit: stub,
			StagePreDeploy: func(ctx context.Context, log *slog.Logger) error {
				return sm.prepareDeploymentRetry(ctx, log)
			},
			StageDeploy: func(ctx context.Context, _ *slog.Logger) error {
				if err := ctx.Err(); err != nil {
					return err
				}

				sm.retryAttempt.Start()

				if deployStage != nil {
					return deployStage(ctx)
				}

				return nil
			},
			StagePostDeploy: stub,
			StageCleanup:    stub,
		},
	}
}

type retryNotificationSender struct {
	count atomic.Int32
}

func (s *retryNotificationSender) Send(notification.Level, string, string, notification.Metadata, ...notification.SendOption) error {
	s.count.Add(1)
	return nil
}

func TestRetryStageRunnerSuppressesBeforeAdmissionAndNotifications(t *testing.T) {
	t.Parallel()
	sm := newRetryTestManager(t, "retry-runner-suppression")
	recordRetryTestFailure(t, sm, StageDeploy)

	sender := &retryNotificationSender{}
	sm.Notifier = sender
	admissionCalled := false
	admit := func(context.Context) (func(), error) {
		admissionCalled = true
		return func() {}, nil
	}
	order := retryStageOrder(sm, func(context.Context) error {
		t.Fatal("suppressed deploy stage was reached")
		return nil
	})

	err := sm.runStageOrder(t.Context(), admit, order)
	if !errors.Is(err, ErrRetrySuppressed) || admissionCalled || sender.count.Load() != 0 || sm.inProgressPosted {
		t.Fatalf("suppression reached admission/notifications: err=%v admitted=%v notified=%d", err, admissionCalled, sender.count.Load())
	}

	if _, failed := sm.lastDeploymentFailure(); !failed {
		t.Fatal("suppressed runner cleared failed outcome")
	}
}

func TestRetrySuppressedPreDeployDoesNotAnnounceRetry(t *testing.T) {
	t.Parallel()

	for _, reason := range []string{"budget_exhausted", "delay"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()

			sm := newRetryTestManager(t, "retry-suppression-log-"+reason)
			if reason == "delay" {
				sm.DeployConfig.Retry.Limit = new(2)
				sm.DeployConfig.Retry.Delay = duration.Duration(time.Hour)
			}

			sm.Docker.Cmd = destroyOwnershipTestCli{apiClient: &destroyOwnershipTestClient{}}
			recordRetryTestFailure(t, sm, StageDeploy)

			var output bytes.Buffer

			sm.Log = slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
			for range 3 {
				if err := sm.RunPreDeployStage(t.Context(), sm.Log); !errors.Is(err, ErrRetrySuppressed) {
					t.Fatalf("expected suppression: %v", err)
				}
			}

			if strings.Contains(output.String(), "last deployment attempt failed, retrying") {
				t.Fatal("suppressed polls falsely announced an admitted retry")
			}

			for _, field := range []string{
				"stack=" + sm.DeployConfig.Name,
				"failed_stage=deploy",
				"failure_reason=broken",
				"retry.reason=" + reason,
				"retry.used=",
				"retry.limit=",
			} {
				if !strings.Contains(output.String(), field) {
					t.Fatalf("expected log field %q: %s", field, output.String())
				}
			}

			if strings.Count(output.String(), "level=WARN") != 1 {
				t.Fatalf("expected one transition warning and repeated debug suppressions: %s", output.String())
			}
		})
	}
}

func TestRetryAdmittedUnlimitedRetryPreservesWarning(t *testing.T) {
	t.Parallel()
	sm := newRetryTestManager(t, "retry-admitted-warning")
	sm.DeployConfig.Retry.Limit = nil
	recordRetryTestFailure(t, sm, StageDeploy)

	var output bytes.Buffer

	log := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if err := sm.prepareDeploymentRetry(t.Context(), log); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(output.String(), "last deployment attempt failed, retrying") {
		t.Fatal("pre-deploy gate must not announce mutation admission")
	}

	sm.retryAttempt.Start()
	sm.logAdmittedDeploymentRetry(t.Context(), log)

	if strings.Count(output.String(), "last deployment attempt failed, retrying") != 1 || !strings.Contains(output.String(), "level=WARN") {
		t.Fatalf("admitted unlimited retry lost its warning: %s", output.String())
	}

	if !strings.Contains(output.String(), "failed_stage=deploy") || !strings.Contains(output.String(), "retry.used=") {
		t.Fatalf("admitted retry log is missing the expected structure: %s", output.String())
	}
}

func TestRetryStageRunnerQueuedCancellationDoesNotSpend(t *testing.T) {
	t.Parallel()
	sm := newRetryTestManager(t, "retry-runner-queued-cancel")
	sm.DeployConfig.Retry.Limit = new(1)
	recordRetryTestFailure(t, sm, StageDeploy)
	before, _ := sm.lastDeploymentFailure()
	order := retryStageOrder(sm, func(context.Context) error {
		t.Fatal("queued cancellation reached Docker mutations")
		return nil
	})
	err := sm.runStageOrder(t.Context(), func(context.Context) (func(), error) {
		return nil, context.Canceled
	}, order)

	after, _ := sm.lastDeploymentFailure()
	if !errors.Is(err, context.Canceled) || before != after {
		t.Fatalf("queued cancellation spent/changed retry state: %v %+v %+v", err, before, after)
	}
	// A canceled queued run must release its target lock.
	release, err := docker.DeploymentFailures.Acquire(t.Context(), sm.deploymentFailureTarget())
	if err != nil {
		t.Fatal(err)
	}

	release()
}

func TestRetryStageRunnerConcurrentFailuresCannotOverspend(t *testing.T) {
	t.Parallel()

	const stack = "retry-runner-concurrent"

	var (
		attempts atomic.Int32
		workers  sync.WaitGroup
	)

	for range 12 {
		sm := newRetryTestManager(t, stack)
		sm.DeployConfig.Retry.Limit = new(2)

		workers.Go(func() {
			err := sm.runStageOrder(t.Context(), nil, retryStageOrder(sm, func(context.Context) error {
				attempts.Add(1)
				return errors.New("broken")
			}))
			if !errors.Is(err, ErrRetrySuppressed) && !notification.WasNotified(err) {
				t.Errorf("unexpected stage result: %v", err)
			}
		})
	}

	workers.Wait()

	if attempts.Load() != 3 {
		t.Fatalf("stage lifecycle admitted %d failures, want three", attempts.Load())
	}
}

func TestRetryStageRunnerShutdownAndHandoverPreserveExhaustion(t *testing.T) {
	t.Parallel()

	for _, cause := range []error{context.Canceled, selfupdate.ErrHandover} {
		t.Run(cause.Error(), func(t *testing.T) {
			t.Parallel()
			sm := newRetryTestManager(t, "retry-shutdown-"+strings.ReplaceAll(cause.Error(), " ", "-"))
			recordRetryTestFailure(t, sm, StageDeploy)
			before, _ := sm.lastDeploymentFailure()
			sm.Origin = syncwindow.OriginManual
			err := sm.runStageOrder(t.Context(), nil, retryStageOrder(sm, func(context.Context) error { return cause }))

			after, _ := sm.lastDeploymentFailure()
			if !errors.Is(err, cause) || before != after {
				t.Fatalf("lifecycle no-op changed exhaustion: %v %+v %+v", err, before, after)
			}
		})
	}
}

func TestRetryStageRunnerSuccessfulDestroyClearsFailure(t *testing.T) {
	t.Parallel()
	sm := newRetryTestManager(t, "retry-destroy-clear")
	recordRetryTestFailure(t, sm, StageDeploy)
	sm.DeployConfig.Destroy.Enabled = true

	order := StageOrder{
		Order: []StageName{StageInit, StageDestroy, StagePostDestroy, StageCleanup},
		Funcs: map[StageName]StageFunc{},
	}
	for _, stage := range order.Order {
		order.Funcs[stage] = func(context.Context, *slog.Logger) error { return nil }
	}

	if err := sm.runStageOrder(t.Context(), nil, order); err != nil {
		t.Fatal(err)
	}

	if _, failed := sm.lastDeploymentFailure(); failed {
		t.Fatal("successful destroy retained exhausted retry budget")
	}
}

func TestRetryDestroyDoesNotInvertDeploymentAdmissionLockOrder(t *testing.T) {
	t.Parallel()
	sm := newRetryTestManager(t, "retry-destroy-lock-order")
	recordRetryTestFailure(t, sm, StageDeploy)
	// A regular run can hold retry admission while waiting for a deployment
	// limiter slot. Destroy already holds its slot before entering stages.
	release, err := docker.DeploymentFailures.Acquire(t.Context(), sm.deploymentFailureTarget())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	sm.DeployConfig.Destroy.Enabled = true

	order := StageOrder{
		Order: []StageName{StageInit, StageDestroy, StagePostDestroy, StageCleanup},
		Funcs: map[StageName]StageFunc{},
	}
	for _, stage := range order.Order {
		order.Funcs[stage] = func(context.Context, *slog.Logger) error { return nil }
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	if err := sm.runStageOrder(ctx, nil, order); err != nil {
		t.Fatalf("destroy must stay outside retry admission to preserve lock ordering: %v", err)
	}
}

func TestRetryStageRunnerPreDeploySkipsLeaveBudgetUntouched(t *testing.T) {
	t.Parallel()

	for _, cause := range []error{ErrSkipDeployment, &SyncWindowBlockedError{Stacks: []string{"web"}}} {
		t.Run(cause.Error(), func(t *testing.T) {
			t.Parallel()
			sm := newRetryTestManager(t, "retry-skip-"+fmt.Sprintf("%T", cause))
			recordRetryTestFailure(t, sm, StageDeploy)
			before, _ := sm.lastDeploymentFailure()
			order := retryStageOrder(sm, nil)

			order.Funcs[StagePreDeploy] = func(context.Context, *slog.Logger) error { return cause }
			if err := sm.runStageOrder(t.Context(), nil, order); !errors.Is(err, ErrSkipDeployment) {
				t.Fatalf("expected skip: %v", err)
			}

			after, _ := sm.lastDeploymentFailure()
			if before != after {
				t.Fatal("skip consumed retry or changed delay")
			}
		})
	}
}

func TestRetryStageManagerOriginPropagation(t *testing.T) {
	t.Parallel()

	sm := newTestStageManager(t)
	for _, origin := range []syncwindow.Origin{"", syncwindow.OriginAutomatic, syncwindow.OriginManual, syncwindow.OriginReconciliation} {
		manager, err := NewStageManager(
			Dependencies{AppConfig: sm.AppConfig, Notifier: sm.Notifier},
			RunInput{Log: sm.Log, JobTrigger: JobTriggerPoll, Origin: origin, Repository: sm.Repository, Docker: sm.Docker, DeployConfig: sm.DeployConfig},
		)
		if err != nil || manager.Origin != origin {
			t.Fatalf("origin %q propagation failed: %v", origin, err)
		}
	}
}

func TestRetryOCIAndMissingResourcePreDeployAreGated(t *testing.T) {
	t.Parallel()

	for _, source := range []config.SourceType{config.SourceTypeGit, config.SourceTypeOCI} {
		t.Run(string(source), func(t *testing.T) {
			t.Parallel()
			sm := newRetryTestManager(t, "retry-empty-runtime-"+string(source))

			sm.Repository.Source = source
			if source == config.SourceTypeOCI {
				sm.Repository.Revision = "sha256:broken-artifact"
			}

			sm.Docker.Cmd = destroyOwnershipTestCli{apiClient: &destroyOwnershipTestClient{}}

			fixtureDir, err := os.MkdirTemp(".", ".retry-runtime-work-")
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { _ = os.RemoveAll(fixtureDir) })

			sourceDir := filepath.Join(fixtureDir, "source")
			if err := os.Mkdir(sourceDir, 0o700); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(filepath.Join(sourceDir, "compose.yaml"), []byte("services:\n  web:\n    image: test:latest\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			sm.Repository.PathInternal = sourceDir
			sm.Repository.PathExternal = sourceDir
			sm.Docker.Project = nil

			sm.Docker.ProjectHash = ""
			if err := sm.RunPreDeployStage(t.Context(), sm.Log); err != nil {
				t.Fatalf("original attempt: %v", err)
			}

			sm.retryAttempt.Start()
			sm.recordDeploymentFailure(StageDeploy, errors.New("broken"))
			sm.Docker.Project = nil
			sm.Docker.ProjectHash = ""

			err = sm.RunPreDeployStage(t.Context(), sm.Log)
			if !errors.Is(err, ErrRetrySuppressed) {
				t.Fatalf("%s with no runtime resources bypassed retry gate: %v", source, err)
			}
		})
	}
}

func TestRetryMergeRetainsMixedReasons(t *testing.T) {
	t.Parallel()

	merged := MergeRetrySuppressed([]*RetrySuppressedError{
		{Stacks: []RetrySuppressedStack{{Name: "same", Context: "a", Reason: "delay"}}},
		{Stacks: []RetrySuppressedStack{{Name: "same", Context: "b", Reason: "budget_exhausted"}}},
	})
	if len(merged.Stacks) != 2 || !slices.Equal([]string{merged.Stacks[0].Reason, merged.Stacks[1].Reason}, []string{"delay", "budget_exhausted"}) {
		t.Fatalf("mixed reasons merged incorrectly: %+v", merged)
	}
}
