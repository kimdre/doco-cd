package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/common/types/duration"
	"github.com/kimdre/doco-cd/internal/config/deploy"
)

func retryTestState() (DeploymentTarget, DeploymentDesiredState) {
	return DeploymentTarget{
		Repository: "github.com/example/app", Stack: "app", ConfigSource: "config-repository", ConfigTarget: "prod",
	}, DeploymentDesiredState{
		Revision: "573a16e", ConfigHash: "config", ProjectHash: "project",
		ImageDigests: map[string]string{"web": "sha256:broken"},
	}
}

func failRetryAttempt(t *testing.T, store *DeploymentFailureStore, target DeploymentTarget, desired DeploymentDesiredState, policy deploy.RetryConfig, manual bool, reason string) {
	t.Helper()

	attempt, suppressed := store.Admit(target, desired, policy, manual)
	if suppressed != nil || attempt == nil {
		t.Fatalf("attempt unexpectedly suppressed: %+v", suppressed)
	}

	attempt.Start()
	attempt.Fail(DeploymentFailure{CommitSHA: desired.Revision, Stage: "deploy", Error: reason}, false)
}

func TestDeploymentRetryBudgets(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		limit    *int
		attempts int
	}{
		{name: "zero means original only", limit: new(0), attempts: 1},
		{name: "two means original and two retries", limit: new(2), attempts: 3},
		{name: "omitted remains unlimited", attempts: 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
			store := NewDeploymentFailureStore(func() time.Time { return now })
			target, desired := retryTestState()

			policy := deploy.RetryConfig{Limit: tc.limit}
			for i := range tc.attempts {
				failRetryAttempt(t, store, target, desired, policy, false, fmt.Sprintf("failure %d", i))

				failure, _ := store.Get(target)
				if failure.RetriesUsed != i {
					t.Fatalf("attempt %d recorded %d retries", i, failure.RetriesUsed)
				}
			}

			_, suppressed := store.Admit(target, desired, policy, false)
			if tc.limit != nil && (suppressed == nil || suppressed.Reason != RetryReasonBudgetExhausted) {
				t.Fatalf("expected exhausted budget, got %+v", suppressed)
			}

			if tc.limit == nil && suppressed != nil {
				t.Fatalf("omitted policy must not suppress: %+v", suppressed)
			}
		})
	}
}

func TestDeploymentRetryDelayUsesActualFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	store := NewDeploymentFailureStore(func() time.Time { return now })
	target, desired := retryTestState()
	policy := deploy.RetryConfig{Limit: new(2), Delay: duration.Duration(5 * time.Minute)}
	failRetryAttempt(t, store, target, desired, policy, false, "first")

	deadline := now.Add(5 * time.Minute)
	for _, delta := range []time.Duration{time.Minute, 4 * time.Minute, 5*time.Minute - time.Nanosecond} {
		now = deadline.Add(delta - 5*time.Minute)

		_, suppressed := store.Admit(target, desired, policy, false)
		if suppressed == nil || suppressed.Reason != RetryReasonDelay || !suppressed.NextRetry.Equal(deadline) {
			t.Fatalf("before deadline: %+v", suppressed)
		}
	}

	now = deadline

	failRetryAttempt(t, store, target, desired, policy, false, "second")

	now = now.Add(5 * time.Minute)

	failRetryAttempt(t, store, target, desired, policy, false, "third")

	now = now.Add(365 * 24 * time.Hour)

	_, suppressed := store.Admit(target, desired, policy, false)
	if suppressed == nil || suppressed.Reason != RetryReasonBudgetExhausted || !suppressed.NextRetry.IsZero() {
		t.Fatalf("delay must not refill exhausted budget: %+v", suppressed)
	}
}

func TestDeploymentRetryDelayOnlyIsUnlimited(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	store := NewDeploymentFailureStore(func() time.Time { return now })
	target, desired := retryTestState()

	policy := deploy.RetryConfig{Delay: duration.Duration(time.Minute)}
	for range 10 {
		failRetryAttempt(t, store, target, desired, policy, false, "failed")

		_, suppressed := store.Admit(target, desired, policy, false)
		if suppressed == nil || suppressed.Reason != RetryReasonDelay || suppressed.RetryLimit != nil {
			t.Fatalf("expected unlimited delayed retries: %+v", suppressed)
		}

		now = now.Add(time.Minute)
	}
}

func TestDeploymentRetryDesiredStateChangesResetBudget(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(*DeploymentDesiredState)
	}{
		{name: "git revision", change: func(d *DeploymentDesiredState) { d.Revision = "fixed" }},
		{name: "oci digest", change: func(d *DeploymentDesiredState) { d.Revision = "sha256:fixed" }},
		{name: "effective configuration", change: func(d *DeploymentDesiredState) { d.ConfigHash = "fixed" }},
		{name: "normalized project", change: func(d *DeploymentDesiredState) { d.ProjectHash = "fixed" }},
		{name: "desired image digest", change: func(d *DeploymentDesiredState) { d.ImageDigests = map[string]string{"web": "sha256:fixed"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := NewDeploymentFailureStore(nil)
			target, desired := retryTestState()
			policy := deploy.RetryConfig{Limit: new(0), Delay: duration.Duration(time.Hour)}
			failRetryAttempt(t, store, target, desired, policy, false, "broken")
			tc.change(&desired)
			failRetryAttempt(t, store, target, desired, policy, false, "still broken")

			failure, _ := store.Get(target)
			if failure.RetriesUsed != 0 {
				t.Fatalf("new desired state retained budget: %+v", failure)
			}

			_, suppressed := store.Admit(target, desired, policy, false)
			if suppressed == nil || suppressed.Reason != RetryReasonBudgetExhausted {
				t.Fatalf("new failure should start its own budget: %+v", suppressed)
			}

			if len(store.failures) != 1 {
				t.Fatalf("expected one active target failure, got %d", len(store.failures))
			}
		})
	}
}

func TestDeploymentRetryUnavailableDigestDoesNotResetBudget(t *testing.T) {
	t.Parallel()

	store := NewDeploymentFailureStore(nil)
	target, desired := retryTestState()
	policy := deploy.RetryConfig{Limit: new(0)}
	failRetryAttempt(t, store, target, desired, policy, false, "broken")
	desired.ImageDigests = nil

	_, suppressed := store.Admit(target, desired, policy, false)
	if suppressed == nil {
		t.Fatal("missing registry result reset exhausted budget")
	}

	desired.ImageDigests = map[string]string{"web": "sha256:fixed"}
	if _, suppressed := store.Admit(target, desired, policy, false); suppressed != nil {
		t.Fatal("genuinely changed digest should reset budget")
	}
}

func TestDeploymentRetryManualFailurePreservesAutomaticBudget(t *testing.T) {
	t.Parallel()

	for _, limit := range []int{0, 2} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
			store := NewDeploymentFailureStore(func() time.Time { return now })
			target, desired := retryTestState()

			policy := deploy.RetryConfig{Limit: &limit, Delay: duration.Duration(time.Hour)}
			for range limit + 1 {
				failRetryAttempt(t, store, target, desired, policy, false, "automatic")

				now = now.Add(time.Hour)
			}

			now = now.Add(time.Second)

			failRetryAttempt(t, store, target, desired, policy, true, "manual failed differently")

			failure, _ := store.Get(target)
			if failure.RetriesUsed != limit || failure.Error != "manual failed differently" || !failure.FailedAt.Equal(now) {
				t.Fatalf("manual failure changed automatic budget or lost details: %+v", failure)
			}

			if _, suppressed := store.Admit(target, desired, policy, false); suppressed == nil {
				t.Fatal("manual failure rearmed automatic retry")
			}

			store.Clear(target)

			if _, failed := store.Get(target); failed {
				t.Fatal("success must clear failure and budget together")
			}

			failRetryAttempt(t, store, target, desired, policy, false, "fresh original")
		})
	}
}

func TestDeploymentRetryManualBypassesDelayWithoutSpending(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	store := NewDeploymentFailureStore(func() time.Time { return now })
	target, desired := retryTestState()
	policy := deploy.RetryConfig{Limit: new(2), Delay: duration.Duration(time.Hour)}
	failRetryAttempt(t, store, target, desired, policy, false, "original")

	now = now.Add(time.Minute)

	failRetryAttempt(t, store, target, desired, policy, true, "manual")

	failure, _ := store.Get(target)
	if failure.RetriesUsed != 0 {
		t.Fatalf("manual retry spent budget: %+v", failure)
	}

	_, suppressed := store.Admit(target, desired, policy, false)
	if suppressed == nil || !suppressed.NextRetry.Equal(now.Add(time.Hour)) {
		t.Fatalf("manual actual failure must update retry delay: %+v", suppressed)
	}
}

func TestDeploymentRetryCancellationPreservesRecoveryAndBudget(t *testing.T) {
	t.Parallel()

	store := NewDeploymentFailureStore(nil)
	target, desired := retryTestState()
	policy := deploy.RetryConfig{Limit: new(1)}
	failRetryAttempt(t, store, target, desired, policy, false, "original")
	before, _ := store.Get(target)
	attempt, _ := store.Admit(target, desired, policy, false)
	attempt.Start()
	attempt.Fail(DeploymentFailure{Stage: "deploy", Error: "canceled"}, true)

	after, _ := store.Get(target)
	if after != before {
		t.Fatalf("shutdown interruption changed actual failure: before=%+v after=%+v", before, after)
	}

	failRetryAttempt(t, store, target, desired, policy, false, "exhausted")
	before, _ = store.Get(target)
	attempt, _ = store.Admit(target, desired, policy, true)
	attempt.Start()
	attempt.Fail(DeploymentFailure{Error: "manual canceled"}, true)

	after, _ = store.Get(target)
	if after != before {
		t.Fatal("canceled manual retry erased exhaustion")
	}

	store.Clear(target)
	attempt, _ = store.Admit(target, desired, deploy.RetryConfig{Limit: new(0)}, false)
	attempt.Start()
	attempt.Fail(DeploymentFailure{Stage: "deploy", Error: "original canceled"}, true)

	failure, failed := store.Get(target)
	if !failed || !failure.FailedAt.IsZero() || failure.RetriesUsed != 0 {
		t.Fatalf("expected untimed recovery marker: %+v", failure)
	}

	if _, suppressed := store.Admit(target, desired, deploy.RetryConfig{Limit: new(0)}, false); suppressed != nil {
		t.Fatal("first interruption must not exhaust original attempt")
	}
}

func TestDeploymentRetryAdmissionWithoutMutationDoesNotSpend(t *testing.T) {
	t.Parallel()

	store := NewDeploymentFailureStore(nil)
	target, desired := retryTestState()
	policy := deploy.RetryConfig{Limit: new(1)}
	failRetryAttempt(t, store, target, desired, policy, false, "original")

	before, _ := store.Get(target)
	for range 5 {
		if _, suppressed := store.Admit(target, desired, policy, false); suppressed != nil {
			t.Fatal("abandoned admission spent a retry")
		}
	}

	after, _ := store.Get(target)
	if after != before {
		t.Fatal("ordinary skip or queued cancellation changed failure")
	}
}

func TestDeploymentRetrySpendsAtDeployEntryAndRefundsHandover(t *testing.T) {
	t.Parallel()

	store := NewDeploymentFailureStore(nil)
	target, desired := retryTestState()
	policy := deploy.RetryConfig{Limit: new(1)}
	failRetryAttempt(t, store, target, desired, policy, false, "original")

	attempt, suppressed := store.Admit(target, desired, policy, false)
	if suppressed != nil {
		t.Fatal("retry unexpectedly suppressed")
	}

	attempt.Start()

	failure, _ := store.Get(target)
	if failure.RetriesUsed != 1 {
		t.Fatal("retry must be charged at actual deploy entry")
	}

	attempt.Cancel()
	attempt.Cancel()

	failure, _ = store.Get(target)
	if failure.RetriesUsed != 0 {
		t.Fatal("handover refund was missing or applied more than once")
	}

	if _, suppressed := store.Admit(target, desired, policy, false); suppressed != nil {
		t.Fatal("handover consumed the only automatic retry")
	}
}

func TestDeploymentRetryDestroyClearsBudgetBeforeQueuedMutation(t *testing.T) {
	t.Parallel()

	store := NewDeploymentFailureStore(nil)
	target, desired := retryTestState()
	policy := deploy.RetryConfig{Limit: new(1)}
	failRetryAttempt(t, store, target, desired, policy, false, "original")

	attempt, suppressed := store.Admit(target, desired, policy, false)
	if suppressed != nil {
		t.Fatal("retry unexpectedly suppressed")
	}

	store.Clear(target)
	attempt.Start()
	attempt.Fail(DeploymentFailure{Stage: "deploy", Error: "failed after successful destruction"}, false)

	failure, _ := store.Get(target)
	if failure.RetriesUsed != 0 {
		t.Fatalf("successful destruction did not clear queued attempt's old budget: %+v", failure)
	}
}

func TestDeploymentRetryTargetIsolationAndDefaultContext(t *testing.T) {
	t.Parallel()

	store := NewDeploymentFailureStore(nil)
	target, desired := retryTestState()
	policy := deploy.RetryConfig{Limit: new(0)}
	failRetryAttempt(t, store, target, desired, policy, false, "original")
	same := target

	same.Context = " DEFAULT "
	if _, suppressed := store.Admit(same, desired, policy, false); suppressed == nil {
		t.Fatal("default contexts must share the same budget")
	}

	for _, change := range []func(*DeploymentTarget){
		func(t *DeploymentTarget) { t.Context = "remote" },
		func(t *DeploymentTarget) { t.Stack = "other" },
		func(t *DeploymentTarget) { t.Repository = "other" },
		func(t *DeploymentTarget) { t.ConfigSource = "other" },
		func(t *DeploymentTarget) { t.ConfigTarget = "other" },
	} {
		other := target
		change(&other)

		if _, suppressed := store.Admit(other, desired, policy, false); suppressed != nil {
			t.Fatalf("unrelated target shares budget: %+v", other)
		}
	}
}

func TestDeploymentRetryConcurrentAttemptsCannotOverspend(t *testing.T) {
	t.Parallel()

	store := NewDeploymentFailureStore(nil)
	target, desired := retryTestState()
	policy := deploy.RetryConfig{Limit: new(2)}

	var (
		attempts atomic.Int32
		workers  sync.WaitGroup
	)
	for range 30 {
		workers.Go(func() {
			release, err := store.Acquire(t.Context(), target)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer release()

			attempt, suppressed := store.Admit(target, desired, policy, false)
			if suppressed != nil {
				return
			}

			attempts.Add(1)
			attempt.Start()
			attempt.Fail(DeploymentFailure{Stage: "post-deploy", Error: "failed"}, false)
		})
	}

	workers.Wait()

	if got := attempts.Load(); got != 3 {
		t.Fatalf("admitted %d attempts, want original plus two retries", got)
	}

	failure, _ := store.Get(target)
	if failure.RetriesUsed != 2 {
		t.Fatalf("unexpected final retry usage: %+v", failure)
	}
}

func TestDeploymentRetryQueuedCancellationReleasesAdmission(t *testing.T) {
	t.Parallel()

	store := NewDeploymentFailureStore(nil)
	target, _ := retryTestState()

	release, err := store.Acquire(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)

	go func() {
		release, err := store.Acquire(ctx, target)
		if release != nil {
			release()
		}

		result <- err
	}()

	cancel()

	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation = %v", err)
	}

	other := target
	other.Context = "remote"

	otherRelease, err := store.Acquire(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}

	otherRelease()
	release()

	nextRelease, err := store.Acquire(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}

	nextRelease()
}

func TestDeploymentRetryRecordsBoundedError(t *testing.T) {
	t.Parallel()

	store := NewDeploymentFailureStore(nil)
	target, desired := retryTestState()
	failRetryAttempt(t, store, target, desired, deploy.RetryConfig{}, false, strings.Repeat("ä", maxRecordedErrorLength+20))

	failure, _ := store.Get(target)
	if len([]rune(failure.Error)) != maxRecordedErrorLength {
		t.Fatal("failure reason exceeded stored rune limit")
	}
}
