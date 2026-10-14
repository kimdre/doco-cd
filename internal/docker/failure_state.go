package docker

import (
	"context"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/lock"
)

/*
Failed deployments must be retried and re-reported until they succeed
(discussion #1702). Container labels cannot hold the outcome: compose stamps
them when it creates the containers, before hooks and later stages run, and
docker cannot change labels on an existing container. So the outcome lives in
daemon memory. A restart drops it and the pre-deploy comparison falls back to
the labels again - accepted trade-off, see the review of #1711.
*/

const (
	// ChangeTypeFailedDeployRetry marks a deployment retry after a recorded
	// failure. A Change of this type carries no service scope: the whole
	// project is force-recreated, because the failed part (e.g. a lifecycle
	// hook) is not attributable to one service.
	ChangeTypeFailedDeployRetry = "failed_deploy_retry"

	// maxRecordedErrorLength caps the stored error text, the full error is in the logs.
	maxRecordedErrorLength = 2000
)

// DeploymentFailure records a failed deployment attempt of one stack.
type DeploymentFailure struct {
	Repository  string
	Stack       string
	CommitSHA   string
	Stage       string
	Error       string
	FailedAt    time.Time
	RetriesUsed int
}

// DeploymentTarget isolates an outcome from identically named deployments in
// other contexts, repositories, or configuration targets.
type DeploymentTarget struct {
	Context      string
	Stack        string
	Repository   string
	ConfigSource string
	ConfigTarget string
}

// normalized returns a copy of the target with whitespace trimmed and the context name normalized.
// This is used for map keys and equality checks.
func (t DeploymentTarget) normalized() DeploymentTarget {
	t.Context = NormalizeContextName(t.Context)
	t.Stack = strings.TrimSpace(t.Stack)
	t.Repository = strings.TrimSpace(t.Repository)
	t.ConfigSource = strings.TrimSpace(t.ConfigSource)
	t.ConfigTarget = strings.TrimSpace(t.ConfigTarget)

	return t
}

// DeploymentDesiredState stores fingerprints, not configuration or resolved
// secrets. ImageDigests contains only service names and immutable digests.
type DeploymentDesiredState struct {
	Revision     string
	ConfigHash   string
	ProjectHash  string
	ImageDigests map[string]string
}

// deploymentFailureState is the internal representation of a failed deployment
// and its desired state. It also tracks the reason for retry suppression to
// avoid repeated notifications for the same reason.
type deploymentFailureState struct {
	failure     DeploymentFailure
	desired     DeploymentDesiredState
	suppression string
}

// DeploymentRetrySuppression is a read-only gate decision for a failed state.
type DeploymentRetrySuppression struct {
	Reason     string
	NextRetry  time.Time
	Failure    DeploymentFailure
	RetryLimit *int
	First      bool
}

const (
	RetryReasonDelay           = "delay"
	RetryReasonBudgetExhausted = "budget_exhausted"
)

// DeploymentFailureStore owns the recovery marker and retry budget together.
// Its target lock precedes the scheduler/deploy lock, and must be held from
// pre-deploy inspection through outcome recording for policy-managed runs.
type DeploymentFailureStore struct {
	mu       sync.Mutex
	failures map[DeploymentTarget]deploymentFailureState
	locks    map[DeploymentTarget]*lock.RepoLock
	now      func() time.Time
}

func NewDeploymentFailureStore(now func() time.Time) *DeploymentFailureStore {
	if now == nil {
		now = time.Now
	}

	return &DeploymentFailureStore{
		failures: make(map[DeploymentTarget]deploymentFailureState),
		locks:    make(map[DeploymentTarget]*lock.RepoLock),
		now:      now,
	}
}

// DeploymentFailures is the process-local store shared by all trigger paths.
var DeploymentFailures = NewDeploymentFailureStore(nil)

func (s *DeploymentFailureStore) Acquire(ctx context.Context, target DeploymentTarget) (func(), error) {
	s.mu.Lock()
	target = target.normalized()

	targetLock := s.locks[target]
	if targetLock == nil {
		targetLock = &lock.RepoLock{}
		s.locks[target] = targetLock
	}
	s.mu.Unlock()

	if !targetLock.LockContext(ctx, "") {
		return nil, ctx.Err()
	}

	// LockContext can grant a queued waiter simultaneously with cancellation.
	if err := ctx.Err(); err != nil {
		targetLock.Unlock()
		return nil, err
	}

	return targetLock.Unlock, nil
}

// truncateDeploymentFailure truncates the error message to avoid storing excessively long strings.
func truncateDeploymentFailure(failure DeploymentFailure) DeploymentFailure {
	if r := []rune(failure.Error); len(r) > maxRecordedErrorLength {
		failure.Error = string(r[:maxRecordedErrorLength])
	}

	return failure
}

// Get returns the recorded failure for a target, if any.
func (s *DeploymentFailureStore) Get(target DeploymentTarget) (DeploymentFailure, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.failures[target.normalized()]

	return state.failure, ok
}

// Clear removes the recorded failure for a target, if any.
func (s *DeploymentFailureStore) Clear(target DeploymentTarget) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.failures, target.normalized())
}

// sameDesiredState checks if the previous and current desired states are equivalent.
func sameDesiredState(previous, current DeploymentDesiredState) bool {
	if previous.Revision != current.Revision ||
		previous.ConfigHash != current.ConfigHash ||
		previous.ProjectHash != current.ProjectHash {
		return false
	}

	// A failed registry lookup is not evidence of an image change. Only a
	// different known immutable digest resets the budget.
	for service, digest := range current.ImageDigests {
		if old := previous.ImageDigests[service]; old != "" && digest != "" && old != digest {
			return false
		}
	}

	return true
}

// DeploymentAttempt is an admitted lifecycle. Admission alone does not spend
// a retry; Start marks entry into the mutating deploy stage.
type DeploymentAttempt struct {
	store       *DeploymentFailureStore
	target      DeploymentTarget
	desired     DeploymentDesiredState
	retriesUsed int
	retry       bool
	manual      bool
	started     bool
	spent       bool
	completed   bool
}

// Admit checks exhaustion before retry delay. The caller must hold Acquire's
// target lock for configured policies until this attempt finishes.
func (s *DeploymentFailureStore) Admit(target DeploymentTarget, desired DeploymentDesiredState, policy deploy.RetryConfig, manual bool) (*DeploymentAttempt, *DeploymentRetrySuppression) {
	s.mu.Lock()
	defer s.mu.Unlock()

	target = target.normalized()
	desired.ImageDigests = maps.Clone(desired.ImageDigests)
	attempt := &DeploymentAttempt{store: s, target: target, desired: desired, manual: manual}

	state, failed := s.failures[target]
	if !failed || !sameDesiredState(state.desired, desired) {
		return attempt, nil
	}

	// Retain previously resolved digests when an inspection is unavailable.
	if attempt.desired.ImageDigests == nil {
		attempt.desired.ImageDigests = make(map[string]string)
	}

	for service, digest := range state.desired.ImageDigests {
		if attempt.desired.ImageDigests[service] == "" {
			attempt.desired.ImageDigests[service] = digest
		}
	}

	state.desired.ImageDigests = maps.Clone(attempt.desired.ImageDigests)
	s.failures[target] = state

	attempt.retriesUsed = state.failure.RetriesUsed

	attempt.retry = !state.failure.FailedAt.IsZero()
	if manual || !attempt.retry {
		return attempt, nil
	}

	decision := &DeploymentRetrySuppression{Failure: state.failure}
	if policy.Limit != nil && state.failure.RetriesUsed >= *policy.Limit {
		decision.Reason = RetryReasonBudgetExhausted
	} else if deadline := state.failure.FailedAt.Add(time.Duration(policy.Delay)); policy.Delay > 0 && s.now().Before(deadline) {
		decision.Reason = RetryReasonDelay
		decision.NextRetry = deadline
	} else {
		return attempt, nil
	}

	if policy.Limit != nil {
		limit := *policy.Limit
		decision.RetryLimit = &limit
	}

	decision.First = state.suppression != decision.Reason
	state.suppression = decision.Reason
	s.failures[target] = state

	return nil, decision
}

// Start marks the attempt as spent and increments the retry count if applicable.
func (a *DeploymentAttempt) Start() {
	if a.started {
		return
	}

	a.store.mu.Lock()
	defer a.store.mu.Unlock()

	// Destroy is outside retry admission and may complete while this run is
	// queued for the deployment limiter. Its success starts a fresh budget.
	state, failed := a.store.failures[a.target]
	if !failed {
		a.retry = false
		a.retriesUsed = 0
	}

	if a.retry && !a.manual {
		a.retriesUsed++
		a.spent = true
		state.failure.RetriesUsed = a.retriesUsed
		a.store.failures[a.target] = state
	}

	a.started = true
}

// cancelLocked refunds the retry budget if the attempt was spent but not completed.
func (a *DeploymentAttempt) cancelLocked() {
	if a.spent && !a.completed {
		if state, failed := a.store.failures[a.target]; failed &&
			state.failure.RetriesUsed == a.retriesUsed && sameDesiredState(state.desired, a.desired) {
			state.failure.RetriesUsed--
			a.store.failures[a.target] = state
		}
	}

	a.completed = true
}

// Cancel refunds entry into deploy when it ends in a lifecycle cancellation,
// ordinary skip, or self-update handover rather than an actual failure.
func (a *DeploymentAttempt) Cancel() {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()

	a.cancelLocked()
}

// Fail updates an actual failure without refilling the automatic budget.
// Shutdown interruptions retain any existing failure unchanged; a first
// interruption creates only a recovery marker, not a timed/budgeted failure.
func (a *DeploymentAttempt) Fail(failure DeploymentFailure, interrupted bool) {
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()

	if interrupted {
		a.cancelLocked()

		if _, exists := s.failures[a.target]; exists {
			return
		}

		failure.FailedAt = time.Time{}
	} else {
		failure.FailedAt = s.now().UTC()
	}

	failure.RetriesUsed = a.retriesUsed
	if interrupted && a.spent {
		failure.RetriesUsed--
	}

	s.failures[a.target] = deploymentFailureState{
		failure: truncateDeploymentFailure(failure),
		desired: a.desired,
	}
	a.completed = true
}

// RecordDeploymentFailure is a convenience function to record a failure without going through
// the full DeploymentAttempt lifecycle. It is used for immediate failure recording scenarios.
func RecordDeploymentFailure(repoName, stack string, failure DeploymentFailure) {
	target := (DeploymentTarget{Repository: repoName, Stack: stack}).normalized()

	DeploymentFailures.mu.Lock()
	defer DeploymentFailures.mu.Unlock()

	DeploymentFailures.failures[target] = deploymentFailureState{
		failure: truncateDeploymentFailure(failure),
		desired: DeploymentDesiredState{Revision: failure.CommitSHA},
	}
}

// GetDeploymentFailure retrieves the recorded failure for a given repository and stack, if any.
func GetDeploymentFailure(repoName, stack string) (DeploymentFailure, bool) {
	return DeploymentFailures.Get(DeploymentTarget{Repository: repoName, Stack: stack})
}

// ClearDeploymentFailure removes the recorded failure for a given repository and stack, if any.
func ClearDeploymentFailure(repoName, stack string) {
	DeploymentFailures.Clear(DeploymentTarget{Repository: repoName, Stack: stack})
}
