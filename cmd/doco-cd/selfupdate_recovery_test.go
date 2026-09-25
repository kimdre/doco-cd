package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

type exitedApplierClient struct {
	finalizerDockerClient
}

// ContainerInspect simulates an applier that exited without recording failure.
func (c *exitedApplierClient) ContainerInspect(_ context.Context, _ string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: container.InspectResponse{State: &container.State{Running: false, ExitCode: 0}}}, nil
}

// TestPredecessorRecoversExitedApplierWithoutTerminalState checks that a
// stopped clone does not leave the predecessor waiting indefinitely.
func TestPredecessorRecoversExitedApplierWithoutTerminalState(t *testing.T) {
	previous := docker.SelfUpdateConfig()

	docker.ConfigureSelfUpdate(docker.SelfUpdateOptions{
		Identity: selfupdate.Identity{ContainerID: "old"},
	})
	t.Cleanup(func() { docker.ConfigureSelfUpdate(previous) })

	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID:          "failed-applier",
		State:       selfupdate.StateApplying,
		Stack:       "self",
		Predecessor: selfupdate.ContainerRef{ID: "old"},
		Applier:     selfupdate.ContainerRef{ID: "clone"},
		Source:      selfupdate.SourceInfo{CommitSHA: "bad", ProjectHash: "changed"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	apiClient := &exitedApplierClient{}
	if err := finalizeAsPredecessor(t.Context(), logger.New(slog.LevelError), apiClient, nil, store, record); err != nil {
		t.Fatal(err)
	}

	if apiClient.stops != 0 || apiClient.removes != 0 {
		t.Errorf("touched the running predecessor: stops=%d, removes=%d", apiClient.stops, apiClient.removes)
	}

	if _, err := store.Load(record.ID); !errors.Is(err, selfupdate.ErrNoRecord) {
		t.Errorf("journal not cleared after failure: %v", err)
	}

	if _, poisoned, err := store.IsPoisoned("", "self", "bad", "changed"); err != nil || !poisoned {
		t.Errorf("failed revision not poisoned: poisoned=%v, err=%v", poisoned, err)
	}
}

// TestRestoredContainerFinalisesRollbackAsSuccessor checks that a container
// recreated by a rollback, which booted before the journal named it restored,
// still reports and clears the rolled-back handover.
func TestRestoredContainerFinalisesRollbackAsSuccessor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		ownID     string
		finalised bool
	}{
		{name: "restored container", ownID: "restored", finalised: true},
		{name: "other container", ownID: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := selfupdate.NewStore(t.TempDir())

			record := selfupdate.Record{
				ID: "drift-rollback", State: selfupdate.StateRolledBack, Stack: "self",
				Predecessor: selfupdate.ContainerRef{ID: "old"},
				Applier:     selfupdate.ContainerRef{ID: "clone"},
				Restored:    selfupdate.ContainerRef{ID: "restored"},
				Source:      selfupdate.SourceInfo{CommitSHA: "bad", ProjectHash: "changed"},
			}
			if err := store.Create(&record); err != nil {
				t.Fatal(err)
			}

			// The journal as the recreated container saw it at boot.
			seen := record
			seen.State = selfupdate.StateApplyDrained
			seen.Restored = selfupdate.ContainerRef{}

			err := finalizeAsSuccessor(t.Context(), logger.New(slog.LevelError), &exitedApplierClient{}, nil, store, seen, tc.ownID)
			if err != nil {
				t.Fatal(err)
			}

			if _, err = store.Load(record.ID); errors.Is(err, selfupdate.ErrNoRecord) != tc.finalised {
				t.Errorf("journal cleared = %t (%v), want %t", errors.Is(err, selfupdate.ErrNoRecord), err, tc.finalised)
			}

			if _, poisoned, err := store.IsPoisoned("", "self", "bad", "changed"); err != nil || poisoned != tc.finalised {
				t.Errorf("poisoned = %t (%v), want %t", poisoned, err, tc.finalised)
			}
		})
	}
}

// TestFailStoppedSelfApplierKeepsRecordedReason checks that recovering a
// stopped applier keeps the failure reason the applier already saved.
func TestFailStoppedSelfApplierKeepsRecordedReason(t *testing.T) {
	t.Parallel()

	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID:          "failed-applier",
		State:       selfupdate.StateApplyReady,
		Predecessor: selfupdate.ContainerRef{ID: "old"},
		Applier:     selfupdate.ContainerRef{ID: "clone"},
		Error:       "load the self stack: boom",
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	failed, err := failStoppedSelfApplier(t.Context(), &exitedApplierClient{}, store, record)
	if err != nil {
		t.Fatal(err)
	}

	if failed.State != selfupdate.StateFailed || !strings.HasPrefix(failed.Error, record.Error+"; ") {
		t.Errorf("recovered applier = %s/%q; want failed with the recorded reason kept", failed.State, failed.Error)
	}
}

type inspectedApplierClient struct {
	finalizerDockerClient
	inspect container.InspectResponse
}

// ContainerInspect returns the configured applier inspect result.
func (c *inspectedApplierClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: c.inspect}, nil
}

// TestFailStoppedSelfApplierFailsExhaustedApplier checks that an applier Docker
// stopped restarting fails the handover, while one with restarts left does not.
func TestFailStoppedSelfApplierFailsExhaustedApplier(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		restarts  int
		wantState selfupdate.State
	}{
		{name: "restarts exhausted", restarts: selfupdate.ApplierMaxRestarts, wantState: selfupdate.StateFailed},
		{name: "restarts left", restarts: selfupdate.ApplierMaxRestarts - 1, wantState: selfupdate.StateApplying},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := selfupdate.NewStore(t.TempDir())

			record := selfupdate.Record{
				ID: "crashing-applier", State: selfupdate.StateApplying,
				Predecessor: selfupdate.ContainerRef{ID: "old"},
				Applier:     selfupdate.ContainerRef{ID: "clone"},
			}
			if err := store.Create(&record); err != nil {
				t.Fatal(err)
			}

			apiClient := &inspectedApplierClient{inspect: container.InspectResponse{
				State:        &container.State{ExitCode: 1},
				RestartCount: tc.restarts,
				HostConfig: &container.HostConfig{RestartPolicy: container.RestartPolicy{
					Name: container.RestartPolicyOnFailure, MaximumRetryCount: selfupdate.ApplierMaxRestarts,
				}},
			}}

			current, err := failStoppedSelfApplier(t.Context(), apiClient, store, record)
			if err != nil || current.State != tc.wantState {
				t.Errorf("crashing applier = %s/%v; want %s", current.State, err, tc.wantState)
			}
		})
	}
}

type restartPolicyClient struct {
	finalizerDockerClient
	store     *selfupdate.Store
	journalID string
	updateErr error
	policies  []container.RestartPolicy
	states    []selfupdate.State
}

// ContainerUpdate records the policy and the journal state it was applied in.
func (c *restartPolicyClient) ContainerUpdate(_ context.Context, _ string, opts client.ContainerUpdateOptions) (client.ContainerUpdateResult, error) {
	record, err := c.store.Load(c.journalID)
	if err != nil {
		return client.ContainerUpdateResult{}, err
	}

	c.policies = append(c.policies, *opts.RestartPolicy)
	c.states = append(c.states, record.State)

	return client.ContainerUpdateResult{}, c.updateErr
}

// TestRecordApplierDrainLiftsRestartLimitFirst checks that the applier restarts
// without a bound before the drain is recorded, and that a failed update
// records the error instead of draining.
func TestRecordApplierDrainLiftsRestartLimitFirst(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		updateErr error
		wantState selfupdate.State
	}{
		{name: "limit lifted", wantState: selfupdate.StateApplyDrained},
		{name: "update fails", updateErr: errdefs.ErrUnavailable, wantState: selfupdate.StateApplyReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := selfupdate.NewStore(t.TempDir())

			record := selfupdate.Record{
				ID: "draining", State: selfupdate.StateApplyReady, Strategy: selfupdate.StrategyApplier,
				Predecessor: selfupdate.ContainerRef{ID: "old"},
				Applier:     selfupdate.ContainerRef{ID: "clone"},
			}
			if err := store.Create(&record); err != nil {
				t.Fatal(err)
			}

			apiClient := &restartPolicyClient{store: store, journalID: record.ID, updateErr: tc.updateErr}

			_, err := recordApplierDrain(t.Context(), apiClient, store, record)
			if (err != nil) != (tc.updateErr != nil) {
				t.Fatalf("recordApplierDrain() error = %v", err)
			}

			for i, policy := range apiClient.policies {
				if policy.Name != container.RestartPolicyOnFailure || policy.MaximumRetryCount != 0 ||
					apiClient.states[i] != selfupdate.StateApplyReady {
					t.Errorf("update %d = %+v in state %s; want unbounded on-failure before the drain", i, policy, apiClient.states[i])
				}
			}

			if len(apiClient.policies) == 0 {
				t.Error("restart limit was not lifted")
			}

			stored, err := store.Load(record.ID)
			if err != nil {
				t.Fatal(err)
			}

			if stored.State != tc.wantState || (stored.Error != "") != (tc.updateErr != nil) {
				t.Errorf("journal = %s/%q; want %s with an error only on failure", stored.State, stored.Error, tc.wantState)
			}
		})
	}
}

// TestAbortedHandoverRetainsJournalUntilSuccessorRemoved checks cleanup
// remains retryable when successor removal fails.
func TestAbortedHandoverRetainsJournalUntilSuccessorRemoved(t *testing.T) {
	t.Parallel()

	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID:        "aborted",
		State:     selfupdate.StateAborted,
		Stack:     "self",
		Successor: selfupdate.ContainerRef{ID: "leftover"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	apiClient := &finalizerDockerClient{removeErr: errors.New("daemon unavailable")}

	log := logger.New(slog.LevelError)
	if err := finalizeAsPredecessor(t.Context(), log, apiClient, nil, store, record); err == nil {
		t.Fatal("expected a failed cleanup to remain retryable")
	}

	if _, err := store.Load(record.ID); err != nil {
		t.Fatalf("record removed despite failed cleanup: %v", err)
	}

	apiClient.removeErr = nil
	if err := finalizeAsPredecessor(t.Context(), log, apiClient, nil, store, record); err != nil {
		t.Fatalf("retry failed: %v", err)
	}

	if _, err := store.Load(record.ID); !errors.Is(err, selfupdate.ErrNoRecord) {
		t.Errorf("record not cleared after successful cleanup: %v", err)
	}
}

// TestCoordinatorWaitsForApplierPreflightAndActiveRuns verifies that the
// predecessor drains only after preflight and waits for existing work.
func TestCoordinatorWaitsForApplierPreflightAndActiveRuns(t *testing.T) {
	clearPendingSelfUpdateRequests()
	t.Cleanup(clearPendingSelfUpdateRequests)

	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID:          "handover",
		State:       selfupdate.StateApplying,
		Strategy:    selfupdate.StrategyApplier,
		Predecessor: selfupdate.ContainerRef{ID: "old"},
		Applier:     selfupdate.ContainerRef{ID: "clone"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})
	started := make(chan struct{})
	release := make(chan struct{})

	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	jobID := runs.Accept("", controlplane.RunTriggerWebhook, controlplane.RunMetadata{})
	if err := runs.Execute(t.Context(), jobID, controlplane.RunExecution{Mode: controlplane.RunAsynchronous},
		func(context.Context) (controlplane.RunResult, error) {
			close(started)
			<-release

			return controlplane.SucceededRun("done"), nil
		}); err != nil {
		t.Fatal(err)
	}

	<-started

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stopped := make(chan struct{}, 1)
	done := make(chan struct{})

	go func() {
		runSelfUpdateCoordinator(ctx, logger.New(slog.LevelError), selfUpdateCoordinatorDeps{
			appConfig: &app.Config{}, client: &finalizerDockerClient{}, store: store,
			runs: runs, stopWork: func() { stopped <- struct{}{} },
		})
		close(done)
	}()

	selfupdate.RequestDrain()

	select {
	case <-stopped:
		t.Fatal("closed admission before applier preflight")
	case <-time.After(50 * time.Millisecond):
	}

	ready, err := store.Update(record, selfupdate.StateApplyReady, selfupdate.ActorApplier)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("did not begin draining after applier became ready")
	}

	current, err := store.Load(record.ID)
	if err != nil || current.State != ready.State {
		t.Fatalf("record before active run finishes = %s/%v; want apply_ready", current.State, err)
	}

	close(release)

	deadline := time.After(3 * time.Second)

	for {
		current, err = store.Load(record.ID)
		if err != nil {
			t.Fatal(err)
		}

		if current.State == selfupdate.StateApplyDrained {
			break
		}

		select {
		case <-deadline:
			t.Fatalf("predecessor did not acknowledge drain: %s", current.State)
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("coordinator did not stop")
	}
}

// TestCoordinatorRecoversTerminalFailureWithoutDraining preserves admission
// when the applier fails before readiness.
func TestCoordinatorRecoversTerminalFailureWithoutDraining(t *testing.T) {
	clearPendingSelfUpdateRequests()
	t.Cleanup(clearPendingSelfUpdateRequests)

	store := selfupdate.NewStore(t.TempDir())
	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stopped := make(chan struct{}, 1)
	done := make(chan struct{})

	go func() {
		runSelfUpdateCoordinator(ctx, logger.New(slog.LevelError), selfUpdateCoordinatorDeps{
			client: &finalizerDockerClient{}, store: store, runs: runs,
			stopWork: func() { stopped <- struct{}{} },
		})
		close(done)
	}()

	for _, id := range []string{"failed-applier", "aborted-scale-out", "staged-applier"} {
		state := selfupdate.StateFailed

		switch id {
		case "aborted-scale-out":
			state = selfupdate.StateAborted
		case "staged-applier":
			state = selfupdate.StateStaged
		}

		record := selfupdate.Record{
			ID: id, State: state, Stack: "self",
			Predecessor: selfupdate.ContainerRef{ID: "old"},
		}
		if err := store.Create(&record); err != nil {
			t.Fatal(err)
		}

		selfupdate.RequestDrain()

		deadline := time.After(3 * time.Second)

		for {
			_, err := store.Load(id)
			if errors.Is(err, selfupdate.ErrNoRecord) {
				break
			}

			if err != nil {
				t.Fatal(err)
			}

			select {
			case <-deadline:
				t.Fatalf("terminal handover %s blocked the live predecessor", id)
			case <-time.After(10 * time.Millisecond):
			}
		}

		select {
		case <-stopped:
			t.Fatal("closed admission while recovering a failed handover")
		default:
		}
	}

	cancel()
	<-done
}

// TestCoordinatorKeepsServingWhenApplierPreflightFails prevents a failed
// preflight from stopping the healthy predecessor.
func TestCoordinatorKeepsServingWhenApplierPreflightFails(t *testing.T) {
	clearPendingSelfUpdateRequests()
	t.Cleanup(clearPendingSelfUpdateRequests)

	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "preflight", State: selfupdate.StateApplying, Strategy: selfupdate.StrategyApplier,
		Stack: "self", Predecessor: selfupdate.ContainerRef{ID: "old"},
		Applier: selfupdate.ContainerRef{ID: "clone"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stopped := make(chan struct{}, 1)
	done := make(chan struct{})

	go func() {
		runSelfUpdateCoordinator(ctx, logger.New(slog.LevelError), selfUpdateCoordinatorDeps{
			client: &finalizerDockerClient{}, store: store, runs: runs,
			stopWork: func() { stopped <- struct{}{} },
		})
		close(done)
	}()

	selfupdate.RequestDrain()

	record.Error = "cannot load self stack"

	failed, err := store.Update(record, selfupdate.StateFailed, selfupdate.ActorApplier)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.After(3 * time.Second)

	for {
		_, err = store.Load(failed.ID)
		if errors.Is(err, selfupdate.ErrNoRecord) {
			break
		}

		if err != nil {
			t.Fatal(err)
		}

		select {
		case <-deadline:
			t.Fatal("preflight failure was not cleaned up")
		case <-time.After(10 * time.Millisecond):
		}
	}

	select {
	case <-stopped:
		t.Fatal("closed admission after applier preflight failed")
	default:
	}

	jobID := runs.Accept("", controlplane.RunTriggerWebhook, controlplane.RunMetadata{})
	if err := runs.Execute(t.Context(), jobID, controlplane.RunExecution{Mode: controlplane.RunSynchronous},
		func(context.Context) (controlplane.RunResult, error) {
			return controlplane.SucceededRun("still serving"), nil
		}); err != nil {
		t.Errorf("live predecessor refused a new run: %v", err)
	}

	cancel()
	<-done
}

// TestPredecessorRestartClosesAdmissionBeforeResumingAppliedDrain checks that
// a restarted predecessor refuses new work during an acknowledged takeover.
func TestPredecessorRestartClosesAdmissionBeforeResumingAppliedDrain(t *testing.T) {
	clearPendingSelfUpdateRequests()
	t.Cleanup(clearPendingSelfUpdateRequests)

	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "handover", State: selfupdate.StateApplyDrained,
		Strategy: selfupdate.StrategyApplier, Predecessor: selfupdate.ContainerRef{ID: "old"},
		Applier: selfupdate.ContainerRef{ID: "clone"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{})

	_, err := finalizeSelfUpdate(t.Context(), logger.New(slog.LevelError), &finalizerDockerClient{},
		nil, store, selfupdate.Identity{ContainerID: "old"}, runs)
	if err != nil {
		t.Fatal(err)
	}

	jobID := runs.Accept("", controlplane.RunTriggerWebhook, controlplane.RunMetadata{})

	err = runs.Execute(t.Context(), jobID, controlplane.RunExecution{Mode: controlplane.RunSynchronous},
		func(context.Context) (controlplane.RunResult, error) {
			t.Fatal("accepted work after an applier drain")
			return controlplane.SucceededRun("unexpected"), nil
		})
	if !errors.Is(err, controlplane.ErrBackgroundWorkClosed) {
		t.Errorf("new work after restart = %v, want admission closed", err)
	}

	select {
	case <-selfupdate.DrainRequests():
	default:
		t.Error("the coordinator was not asked to converge the drained handover")
	}
}

// TestSuccessorCannotFinaliseWhileApplierWaitsForDrain requires explicit
// predecessor acknowledgment before a successor completes the handover.
func TestSuccessorCannotFinaliseWhileApplierWaitsForDrain(t *testing.T) {
	t.Parallel()

	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "waiting", State: selfupdate.StateApplyReady,
		Predecessor: selfupdate.ContainerRef{ID: "old"},
		Applier:     selfupdate.ContainerRef{ID: "clone"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	err := finalizeAsSuccessor(ctx, logger.New(slog.LevelError), &finalizerDockerClient{}, nil, store, record, "new")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("finalise while applier waits = %v; want to wait for the applier", err)
	}

	current, err := store.Load(record.ID)
	if err != nil || current.State != record.State {
		t.Errorf("journal after attempted early finalisation = %s/%v", current.State, err)
	}
}

// TestStagedPredecessorFindsApplierWithoutJournalID checks recovery of a
// clone created just before its ID could be persisted.
func TestStagedPredecessorFindsApplierWithoutJournalID(t *testing.T) {
	t.Parallel()

	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{ID: "staging", State: selfupdate.StateStaged, Stack: "self"}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	apiClient := &finalizerDockerClient{
		list: []container.Summary{
			{ID: "orphan", Labels: map[string]string{docker.SelfApplierLabel: record.ID}},
			{ID: "different-attempt", Labels: map[string]string{docker.SelfApplierLabel: "other"}},
		},
		removeErr: errors.New("daemon unavailable"),
	}

	log := logger.New(slog.LevelError)
	if err := finalizeAsPredecessor(t.Context(), log, apiClient, nil, store, record); err == nil {
		t.Fatal("expected failed applier removal to retain the recovery journal")
	}

	if _, err := store.Load(record.ID); err != nil {
		t.Fatalf("removed journal despite orphan applier: %v", err)
	}

	apiClient.removeErr = nil
	if err := finalizeAsPredecessor(t.Context(), log, apiClient, nil, store, record); err != nil {
		t.Fatal(err)
	}

	if len(apiClient.removedIDs) != 2 || apiClient.removedIDs[0] != "orphan" ||
		apiClient.removedIDs[1] != "orphan" {
		t.Errorf("removed containers = %v; want only this attempt's applier", apiClient.removedIDs)
	}

	if _, err := store.Load(record.ID); !errors.Is(err, selfupdate.ErrNoRecord) {
		t.Errorf("journal retained after successful cleanup: %v", err)
	}
}

// TestApplierReadinessWaitsForPendingRecovery prevents draining while
// applier failure recovery is pending.
func TestApplierReadinessWaitsForPendingRecovery(t *testing.T) {
	t.Parallel()

	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "recovering", State: selfupdate.StateApplyReady, Error: "rollback not yet complete",
		Applier: selfupdate.ContainerRef{ID: "clone"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	if _, err := waitForApplierReady(ctx, logger.New(slog.LevelError), &finalizerDockerClient{}, store, record); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("drain allowed with pending applier recovery: %v", err)
	}

	if current, err := store.Load(record.ID); err != nil || current.State != selfupdate.StateApplyReady {
		t.Errorf("journal changed during pending recovery: %s/%v", current.State, err)
	}
}

// clearPendingSelfUpdateRequests resets queued drain signals between tests.
func clearPendingSelfUpdateRequests() {
	for {
		select {
		case <-selfupdate.DrainRequests():
		default:
			return
		}
	}
}
