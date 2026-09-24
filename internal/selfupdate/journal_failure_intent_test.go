package selfupdate

import (
	"errors"
	"testing"
)

// TestPendingRecoveryCannotBecomeApplyReadyOrBeClearedByStaleSave keeps a
// failure intent from being lost to concurrent readiness or stale writes.
func TestPendingRecoveryCannotBecomeApplyReadyOrBeClearedByStaleSave(t *testing.T) {
	store := newTestStore(t)
	record := newTestRecord("pending-error")

	record.State = StateApplying
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}

	stale := *record

	record.Error = "preflight failed; restore also failed: Docker unavailable"
	if err := store.Save(*record); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Update(stale, StateApplyReady, ActorApplier); !errors.Is(err, ErrStaleRecord) {
		t.Errorf("ready with a stale error = %v; want ErrStaleRecord", err)
	}

	if err := store.Save(stale); !errors.Is(err, ErrStaleRecord) {
		t.Errorf("clearing an unseen recovery error = %v; want ErrStaleRecord", err)
	}

	current, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = store.Update(current, StateApplyReady, ActorApplier); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("ready with known pending recovery = %v; want ErrInvalidTransition", err)
	}

	after, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if after.State != StateApplying || after.Error != record.Error {
		t.Errorf("pending failure was overwritten: %+v", after)
	}
}
