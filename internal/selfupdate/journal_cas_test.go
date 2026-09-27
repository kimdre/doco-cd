package selfupdate

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStoreRejectsStaleTransitionsAndSaves prevents outdated writers from
// replacing more recent journal state.
func TestStoreRejectsStaleTransitionsAndSaves(t *testing.T) {
	store := newTestStore(t)
	record := newTestRecord("race")

	record.State = StateApplyReady
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}

	applier, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	predecessor := applier

	failed, err := store.Update(applier, StateFailed, ActorApplier)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = NewStore(filepath.Dir(store.Root())).Update(predecessor, StateApplyDrained, ActorPredecessor); !errors.Is(err, ErrStaleRecord) {
		t.Errorf("acknowledge stale readiness: got %v; want ErrStaleRecord", err)
	}

	predecessor.Error = "stale setup error"
	if err = store.Save(predecessor); !errors.Is(err, ErrStaleRecord) {
		t.Errorf("save stale readiness: got %v; want ErrStaleRecord", err)
	}

	got, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if got.State != StateFailed || got.Error != failed.Error || len(got.History) != len(failed.History) {
		t.Errorf("stale writer changed terminal record: %+v", got)
	}

	if err = store.Remove(record.ID); err != nil {
		t.Fatal(err)
	}

	if err = store.Save(failed); !errors.Is(err, ErrStaleRecord) {
		t.Errorf("save removed record: got %v; want ErrStaleRecord", err)
	}
}

// TestStoreUpdateCrossProcessCAS verifies competing Store instances cannot
// both advance the same journal record.
func TestStoreUpdateCrossProcessCAS(t *testing.T) {
	if root := os.Getenv("DOCO_CD_CAS_CHILD_ROOT"); root != "" {
		store := NewStore(root)

		record, err := store.Load("race")
		if err != nil {
			t.Fatal(err)
		}

		_, _ = fmt.Fprintln(os.Stdout, "ready")

		if _, err = io.ReadAll(os.Stdin); err != nil {
			t.Fatal(err)
		}

		_, err = store.Update(record, StateApplyDrained, ActorPredecessor)
		if !errors.Is(err, ErrStaleRecord) {
			t.Fatalf("competing update = %v; want ErrStaleRecord", err)
		}

		_, _ = fmt.Fprintln(os.Stdout, "stale")

		return
	}

	root := t.TempDir()
	store := NewStore(root)
	record := newTestRecord("race")

	record.State = StateApplyReady
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}

	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestStoreUpdateCrossProcessCAS$")

	cmd.Env = append(os.Environ(), "DOCO_CD_CAS_CHILD_ROOT="+root)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer

	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(stdout)
	if line, readErr := reader.ReadString('\n'); readErr != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("child did not load old state: %q (%v); stderr: %s", line, readErr, stderr.String())
	}

	unlock, err := store.lockJournal()
	if err != nil {
		t.Fatal(err)
	}

	if err = stdin.Close(); err != nil {
		unlock()
		t.Fatal(err)
	}

	done := make(chan string, 1)

	go func() {
		line, _ := reader.ReadString('\n')
		done <- strings.TrimSpace(line)
	}()

	select {
	case line := <-done:
		unlock()
		t.Fatalf("child wrote %q while cross-process lock was held", line)
	case <-time.After(100 * time.Millisecond):
	}

	record.State = StateFailed

	record.History = append(record.History, Transition{State: StateFailed, Actor: ActorApplier, At: time.Now().UTC()})
	if err = store.write(*record); err != nil {
		unlock()
		t.Fatal(err)
	}

	unlock()

	select {
	case line := <-done:
		if line != "stale" {
			t.Errorf("child result = %q; want stale (stderr: %s)", line, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child did not finish after journal lock was released")
	}

	if err = cmd.Wait(); err != nil {
		t.Fatalf("child failed: %v; stderr: %s", err, stderr.String())
	}

	current, err := store.Load(record.ID)
	if err != nil || current.State != StateFailed {
		t.Fatalf("journal after competing processes = %s/%v; want failed", current.State, err)
	}
}

// TestStoreRejectsOutdatedSameStateHistory checks history as well as state
// when rejecting a stale journal update.
func TestStoreRejectsOutdatedSameStateHistory(t *testing.T) {
	store := newTestStore(t)
	record := newTestRecord("race")

	record.State = StateApplying
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}

	outdated := *record
	if _, err := store.Update(*record, StateApplying, ActorApplier); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Update(outdated, StateApplyReady, ActorApplier); !errors.Is(err, ErrStaleRecord) {
		t.Errorf("stale same-state history update = %v; want ErrStaleRecord", err)
	}

	if err := store.Save(outdated); !errors.Is(err, ErrStaleRecord) {
		t.Errorf("stale same-state history save = %v; want ErrStaleRecord", err)
	}
}

// TestStorePendingFailureBlocksHandshake keeps a persisted recovery request
// from being lost to a concurrent handshake transition or stale save.
func TestStorePendingFailureBlocksHandshake(t *testing.T) {
	for _, tc := range []struct {
		name  string
		from  State
		to    State
		actor Actor
	}{
		{name: "applier cannot become ready", from: StateApplying, to: StateApplyReady, actor: ActorApplier},
		{name: "predecessor cannot drain", from: StateApplyReady, to: StateApplyDrained, actor: ActorPredecessor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			record := newTestRecord("pending-error")

			record.State = tc.from
			if err := store.Create(record); err != nil {
				t.Fatal(err)
			}

			stale := *record

			record.Error = "applier failed; restore also failed"
			if err := store.Save(*record); err != nil {
				t.Fatal(err)
			}

			if _, err := store.Update(stale, tc.to, tc.actor); !errors.Is(err, ErrStaleRecord) {
				t.Errorf("transition without the error = %v; want ErrStaleRecord", err)
			}

			if err := store.Save(stale); !errors.Is(err, ErrStaleRecord) {
				t.Errorf("clearing an unseen error = %v; want ErrStaleRecord", err)
			}

			current, err := store.Load(record.ID)
			if err != nil {
				t.Fatal(err)
			}

			if _, err = store.Update(current, tc.to, tc.actor); !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("transition with known error = %v; want ErrInvalidTransition", err)
			}

			after, err := store.Load(record.ID)
			if err != nil {
				t.Fatal(err)
			}

			if after.State != tc.from || after.Error != record.Error {
				t.Errorf("pending failure was overwritten: %+v", after)
			}
		})
	}
}

// TestStoreSaveThenUpdateRetainsReason preserves a recorded failure reason
// across subsequent state transitions.
func TestStoreSaveThenUpdateRetainsReason(t *testing.T) {
	for _, tc := range []struct {
		name  string
		from  State
		to    State
		actor Actor
	}{
		{name: "started successor rollback", from: StateStarted, to: StateAborted, actor: ActorPredecessor},
		{name: "applier ready failure", from: StateApplyReady, to: StateFailed, actor: ActorApplier},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			record := newTestRecord("handover")

			record.State = tc.from
			if err := store.Create(record); err != nil {
				t.Fatal(err)
			}

			record.Error = "recovery reason"
			if err := store.Save(*record); err != nil {
				t.Fatal(err)
			}

			updated, err := store.Update(*record, tc.to, tc.actor)
			if err != nil {
				t.Fatalf("update using pre-Save UpdatedAt: %v", err)
			}

			loaded, err := store.Load(record.ID)
			if err != nil {
				t.Fatal(err)
			}

			if loaded.State != tc.to || loaded.Error != record.Error ||
				len(loaded.History) != len(updated.History) {
				t.Errorf("saved reason lost after transition: %+v", loaded)
			}
		})
	}
}

// TestStoreRemoveIfUnchangedProtectsAdvancedJournal prevents cleanup from
// deleting a record updated by another process.
func TestStoreRemoveIfUnchangedProtectsAdvancedJournal(t *testing.T) {
	store := newTestStore(t)
	record := newTestRecord("stage")

	record.State = StateStaged
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}

	outdated := *record

	updated, err := store.Update(*record, StateApplying, ActorPredecessor)
	if err != nil {
		t.Fatal(err)
	}

	if err = store.RemoveIfUnchanged(outdated); !errors.Is(err, ErrStaleRecord) {
		t.Errorf("remove after transition = %v; want ErrStaleRecord", err)
	}

	updated.Applier.ID = "another-clone"
	if err = store.Save(updated); err != nil {
		t.Fatal(err)
	}

	updated.Applier.ID = "old-clone"
	if err = store.RemoveIfUnchanged(updated); !errors.Is(err, ErrStaleRecord) {
		t.Errorf("remove another clone's journal = %v; want ErrStaleRecord", err)
	}

	if active, err := store.Active(); err != nil || active == nil || active.Applier.ID != "another-clone" {
		t.Errorf("recovery journal lost: %+v (%v)", active, err)
	}
}

// TestStoreRemoveIfUnchangedAllowsUnpersistedRemovedClone permits cleanup
// when clone creation never advanced the journal.
func TestStoreRemoveIfUnchangedAllowsUnpersistedRemovedClone(t *testing.T) {
	store := newTestStore(t)
	record := newTestRecord("stage")

	record.State = StateStaged
	if err := store.Create(record); err != nil {
		t.Fatal(err)
	}

	record.Applier.ID = "already-removed-clone"
	if err := store.RemoveIfUnchanged(*record); err != nil {
		t.Fatal(err)
	}

	if active, err := store.Active(); err != nil || active != nil {
		t.Errorf("removed clone still owns a journal: %+v (%v)", active, err)
	}
}
