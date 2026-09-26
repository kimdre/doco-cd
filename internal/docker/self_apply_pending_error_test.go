package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// TestFinishSelfApplyFailurePreservesConcurrentRecoveryReason keeps the
// original error when another process starts rollback concurrently.
func TestFinishSelfApplyFailurePreservesConcurrentRecoveryReason(t *testing.T) {
	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "handover", State: selfupdate.StateApplying,
		Predecessor: selfupdate.ContainerRef{ID: "old"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	stale := record

	record.Error = "existing failure intent"
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}

	fake := &selfApplyTestClient{}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := finishSelfApplyFailure(t.Context(), fake, store, stale, errors.New("later stale setup error"), log); err != nil {
		t.Fatal(err)
	}

	after, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if after.State != selfupdate.StateFailed || after.Error != record.Error {
		t.Errorf("concurrent failure intent was overwritten: %s/%q", after.State, after.Error)
	}
}

type failureDuringReadyClient struct {
	*selfApplyTestClient
	store     *selfupdate.Store
	recordID  string
	triggered bool
}

// ContainerInspect injects a pending failure during readiness checking.
func (c *failureDuringReadyClient) ContainerInspect(ctx context.Context, id string, opts client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	result, err := c.selfApplyTestClient.ContainerInspect(ctx, id, opts)
	if err != nil || c.triggered {
		return result, err
	}

	c.triggered = true

	record, err := c.store.Load(c.recordID)
	if err != nil {
		return result, err
	}

	record.Error = "predecessor recovery was requested"

	return result, c.store.Save(record)
}

// TestReadyAndWaitSelfApplyRecoversErrorSavedDuringPreflight checks recovery
// when the failure reason was persisted before readiness.
func TestReadyAndWaitSelfApplyRecoversErrorSavedDuringPreflight(t *testing.T) {
	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "handover", State: selfupdate.StateApplying,
		Predecessor: selfupdate.ContainerRef{ID: "old"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	stale := record

	record.Error = "source reload failed; restore also failed: Docker unavailable"
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}

	fake := &selfApplyTestClient{}

	pending, err := readyAndWaitSelfApply(t.Context(), fake, store, stale)
	if err == nil || pending.State != selfupdate.StateApplying || pending.Error != record.Error {
		t.Fatalf("ready with concurrent failure = %s/%q (%v); want pending recovery", pending.State, pending.Error, err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := finishSelfApplyFailure(t.Context(), fake, store, pending, pendingSelfApplyFailure(pending), log); err != nil {
		t.Fatalf("finish saved recovery intent: %v", err)
	}

	after, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if after.State != selfupdate.StateFailed || after.Error != "source reload failed" || fake.started != 0 {
		t.Errorf("preflight overwrote pending recovery: %+v; start calls %d", after, fake.started)
	}
}

// TestReadyAndWaitSelfApplyDetectsFailureSavedWhileWaiting ensures the clone
// notices a recovery request while it waits for the predecessor.
func TestReadyAndWaitSelfApplyDetectsFailureSavedWhileWaiting(t *testing.T) {
	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "handover", State: selfupdate.StateApplyReady,
		Predecessor: selfupdate.ContainerRef{ID: "old"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	fake := &failureDuringReadyClient{
		selfApplyTestClient: &selfApplyTestClient{}, store: store, recordID: record.ID,
	}

	ready, err := readyAndWaitSelfApply(t.Context(), fake, store, record)
	if err == nil || !fake.triggered || ready.State != selfupdate.StateApplyReady ||
		ready.Error != "predecessor recovery was requested" {
		t.Fatalf("wait did not notice saved failure: %s/%q (%v)", ready.State, ready.Error, err)
	}
}

// TestApplySelfUpdateRetriesPendingRecoveryInsteadOfDraining checks that a
// restarted clone resumes rollback rather than requesting another drain.
func TestApplySelfUpdateRetriesPendingRecoveryInsteadOfDraining(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   selfupdate.State
		stopped bool
		want    selfupdate.State
	}{
		{name: "applying running predecessor", state: selfupdate.StateApplying, want: selfupdate.StateFailed},
		{name: "applying stopped predecessor", state: selfupdate.StateApplying, stopped: true, want: selfupdate.StateRolledBack},
		{name: "ready running predecessor", state: selfupdate.StateApplyReady, want: selfupdate.StateFailed},
		{name: "ready stopped predecessor", state: selfupdate.StateApplyReady, stopped: true, want: selfupdate.StateRolledBack},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := selfupdate.NewStore(t.TempDir())
			record := selfupdate.Record{
				ID: "handover", State: tc.state,
				Error:       "secret provider unavailable; restore also failed: Docker unavailable",
				Predecessor: selfupdate.ContainerRef{ID: "old"},
			}

			if err := store.Create(&record); err != nil {
				t.Fatal(err)
			}

			fake := &selfApplyTestClient{stopped: tc.stopped}
			if err := ApplySelfUpdate(t.Context(), selfApplyTestCli{apiClient: fake}, ApplySelfOptions{
				Store: store, JournalID: record.ID, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}); err != nil {
				t.Fatalf("resume pending rollback: %v", err)
			}

			after, err := store.Load(record.ID)
			if err != nil {
				t.Fatal(err)
			}

			if after.State != tc.want || after.Error != "secret provider unavailable" ||
				fake.stopped || (tc.stopped && fake.started != 1) ||
				(!tc.stopped && fake.started != 0) {
				t.Errorf("retry = state %s, error %q, stopped %v, started %d; want %s with original reason",
					after.State, after.Error, fake.stopped, fake.started, tc.want)
			}

			for _, transition := range after.History {
				if transition.State == selfupdate.StateApplyDrained ||
					(tc.state == selfupdate.StateApplying && transition.State == selfupdate.StateApplyReady) {
					t.Errorf("applier drained after a pending failure: %+v", after.History)
				}
			}

			if strings.Contains(after.Error, "restore also failed") {
				t.Errorf("kept a stale restore error after recovery: %q", after.Error)
			}
		})
	}
}
