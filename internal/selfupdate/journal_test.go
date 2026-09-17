package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	return NewStore(t.TempDir())
}

func newTestRecord(id string) *Record {
	return &Record{
		ID:       id,
		State:    StateStaged,
		Strategy: StrategyScaleOut,
		Stack:    "doco-cd",
		Context:  "default",
		Service:  "app",
		Predecessor: ContainerRef{
			ID:     "abc123",
			Name:   "doco-cd-app-1",
			Number: 1,
		},
		Labels: map[string]string{"com.docker.compose.project": "doco-cd"},
	}
}

func TestStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	record := newTestRecord("run-1")

	if err := store.Create(record); err != nil {
		t.Fatalf("create: %v", err)
	}

	if record.Version != RecordVersion {
		t.Errorf("version = %d, want %d", record.Version, RecordVersion)
	}

	updated, err := store.Update(*record, StateStarted, ActorPredecessor)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	loaded, err := store.Load("run-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.State != StateStarted {
		t.Errorf("state = %q, want %q", loaded.State, StateStarted)
	}

	if len(loaded.History) != 2 {
		t.Errorf("history has %d entries, want 2", len(loaded.History))
	}

	if !loaded.UpdatedAt.After(loaded.CreatedAt) && loaded.UpdatedAt.Equal(loaded.CreatedAt) {
		t.Logf("UpdatedAt did not move, clock resolution: %v", loaded.UpdatedAt)
	}

	active, err := store.Active()
	if err != nil {
		t.Fatalf("active: %v", err)
	}

	if active == nil || active.ID != "run-1" {
		t.Fatalf("active = %v, want run-1", active)
	}

	if err = store.Remove(updated.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}

	active, err = store.Active()
	if err != nil {
		t.Fatalf("active after remove: %v", err)
	}

	if active != nil {
		t.Errorf("active = %v, want nil", active)
	}
}

func TestStoreTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		from    State
		to      State
		actor   Actor
		wantErr bool
	}{
		{name: "staged to started", from: StateStaged, to: StateStarted, actor: ActorPredecessor},
		{name: "staged to applying", from: StateStaged, to: StateApplying, actor: ActorPredecessor},
		{name: "staged to aborted", from: StateStaged, to: StateAborted, actor: ActorPredecessor},
		{name: "started to handover", from: StateStarted, to: StateHandover, actor: ActorPredecessor},
		{name: "started to aborted", from: StateStarted, to: StateAborted, actor: ActorPredecessor},
		{name: "handover to drained", from: StateHandover, to: StateDrained, actor: ActorPredecessor},
		{name: "handover to finalising", from: StateHandover, to: StateFinalising, actor: ActorSuccessor},
		{name: "drained to finalising", from: StateDrained, to: StateFinalising, actor: ActorSuccessor},
		{name: "applying to applying", from: StateApplying, to: StateApplying, actor: ActorApplier},
		{name: "applying to applied", from: StateApplying, to: StateApplied, actor: ActorApplier},
		{name: "applying to rolled back", from: StateApplying, to: StateRolledBack, actor: ActorApplier},
		{name: "applying to failed", from: StateApplying, to: StateFailed, actor: ActorApplier},
		{name: "applied to finalising", from: StateApplied, to: StateFinalising, actor: ActorSuccessor},

		{name: "staged to applied is not allowed", from: StateStaged, to: StateApplied, actor: ActorPredecessor, wantErr: true},
		{name: "applied to staged is not allowed", from: StateApplied, to: StateStaged, actor: ActorSuccessor, wantErr: true},
		{name: "drained by successor is not allowed", from: StateHandover, to: StateDrained, actor: ActorSuccessor, wantErr: true},
		{name: "finalising to handover is not allowed", from: StateFinalising, to: StateHandover, actor: ActorSuccessor, wantErr: true},
		{name: "unknown source state", from: State("nonsense"), to: StateStaged, actor: ActorPredecessor, wantErr: true},
		{name: "empty target state", from: StateStaged, to: State(""), actor: ActorPredecessor, wantErr: true},
		{name: "applying by predecessor is not allowed", from: StateApplying, to: StateApplied, actor: ActorPredecessor, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := newTestStore(t)
			record := newTestRecord("run-1")
			record.State = tt.from

			if err := store.Create(record); err != nil {
				t.Fatalf("create: %v", err)
			}

			_, err := store.Update(*record, tt.to, tt.actor)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidTransition) {
					t.Errorf("err = %v, want ErrInvalidTransition", err)
				}

				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestStoreActiveRejectsMultiple(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	for _, id := range []string{"run-1", "run-2"} {
		if err := store.Create(newTestRecord(id)); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	_, err := store.Active()
	if err == nil {
		t.Fatal("want an error for two active records")
	}

	for _, id := range []string{"run-1", "run-2"} {
		if got := err.Error(); !strings.Contains(got, id) {
			t.Errorf("error %q does not name %s", got, id)
		}
	}
}

func TestStoreQuarantinesMalformed(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	if err := os.MkdirAll(store.Root(), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	path := filepath.Join(store.Root(), "broken.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	active, err := store.Active()
	if err != nil {
		t.Fatalf("active: %v", err)
	}

	if active != nil {
		t.Errorf("active = %v, want nil", active)
	}

	if _, err = os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("malformed record was not quarantined: %v", err)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	if _, ok, err := store.ReadSnapshot("run-1"); err != nil || ok {
		t.Fatalf("ReadSnapshot on empty store: ok=%v err=%v", ok, err)
	}

	snap := container.InspectResponse{}
	snap.Name = "/doco-cd-app-1"
	snap.Image = "sha256:deadbeef"

	if err := store.WriteSnapshot("run-1", snap); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	got, ok, err := store.ReadSnapshot("run-1")
	if err != nil || !ok {
		t.Fatalf("read snapshot: ok=%v err=%v", ok, err)
	}

	if got.Name != snap.Name || got.Image != snap.Image {
		t.Errorf("snapshot round trip lost data: %+v", got)
	}

	// A snapshot must not be mistaken for an active record.
	active, err := store.Active()
	if err != nil {
		t.Fatalf("active: %v", err)
	}

	if active != nil {
		t.Errorf("snapshot file was read as an active record: %v", active)
	}
}

func TestLoadMissingRecord(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	if _, err := store.Load("nope"); !errors.Is(err, ErrNoRecord) {
		t.Errorf("err = %v, want ErrNoRecord", err)
	}
}
