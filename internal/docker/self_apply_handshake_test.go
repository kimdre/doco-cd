package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/selfupdate"
)

type drainDuringRecoveryClient struct {
	*selfApplyTestClient
	store    *selfupdate.Store
	recordID string
	drained  bool
}

// ContainerInspect lets the mock predecessor drain during an applier recovery.
func (c *drainDuringRecoveryClient) ContainerInspect(ctx context.Context, id string, opts client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	result, err := c.selfApplyTestClient.ContainerInspect(ctx, id, opts)
	if err != nil || c.drained {
		return result, err
	}

	c.drained = true

	record, err := c.store.Load(c.recordID)
	if err != nil {
		return result, err
	}

	_, err = c.store.Update(record, selfupdate.StateApplyDrained, selfupdate.ActorPredecessor)

	return result, err
}

// TestReadyAndWaitSelfApplyRequiresPredecessorDrain verifies that readiness
// alone does not authorize changing the predecessor container.
func TestReadyAndWaitSelfApplyRequiresPredecessorDrain(t *testing.T) {
	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "handover", State: selfupdate.StateApplying,
		Predecessor: selfupdate.ContainerRef{ID: "old"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	fake := &selfApplyTestClient{}

	type result struct {
		record selfupdate.Record
		err    error
	}

	done := make(chan result, 1)

	go func() {
		after, err := readyAndWaitSelfApply(t.Context(), fake, store, record)
		done <- result{record: after, err: err}
	}()

	var ready selfupdate.Record

	deadline := time.After(3 * time.Second)

	for ready.State != selfupdate.StateApplyReady {
		var err error

		ready, err = store.Load(record.ID)
		if err != nil {
			t.Fatal(err)
		}

		select {
		case <-done:
			t.Fatal("applier returned before the predecessor drained")
		case <-deadline:
			t.Fatal("applier never recorded readiness")
		case <-time.After(10 * time.Millisecond):
		}
	}

	select {
	case <-done:
		t.Fatal("applier advanced before predecessor drain")
	default:
	}

	drained, err := store.Update(ready, selfupdate.StateApplyDrained, selfupdate.ActorPredecessor)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got.err != nil || got.record.State != drained.State {
			t.Errorf("applier after drain = %s/%v; want apply_drained", got.record.State, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("applier did not advance after predecessor drain")
	}
}

// TestReadyAndWaitSelfApplyRecoversMissingPredecessor checks recovery when
// the expected predecessor is no longer present.
func TestReadyAndWaitSelfApplyRecoversMissingPredecessor(t *testing.T) {
	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "handover", State: selfupdate.StateApplying,
		Predecessor: selfupdate.ContainerRef{ID: "old"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	fake := &selfApplyTestClient{stopped: true}

	ready, err := readyAndWaitSelfApply(t.Context(), fake, store, record)
	if err == nil || ready.State != selfupdate.StateApplyReady {
		t.Fatalf("applier with stopped predecessor = %s/%v; want ready and recovery error", ready.State, err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err = finishSelfApplyFailure(t.Context(), fake, store, ready, err, log); err != nil {
		t.Fatalf("restore predecessor after aborted drain: %v", err)
	}

	after, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if after.State != selfupdate.StateRolledBack || after.Restored.ID != "old" || fake.stopped {
		t.Errorf("recovered predecessor = %+v, stopped=%v; want running and rolled back", after, fake.stopped)
	}
}

// drainThenStopClient records the predecessor's drain and reports it stopped,
// as if it crashed right after acknowledging the drain.
type drainThenStopClient struct {
	*selfApplyTestClient
	store    *selfupdate.Store
	recordID string
}

// ContainerInspect drains and stops the mock predecessor on first inspection.
func (c *drainThenStopClient) ContainerInspect(ctx context.Context, id string, opts client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if !c.stopped {
		record, err := c.store.Load(c.recordID)
		if err != nil {
			return client.ContainerInspectResult{}, err
		}

		if _, err = c.store.Update(record, selfupdate.StateApplyDrained, selfupdate.ActorPredecessor); err != nil {
			return client.ContainerInspectResult{}, err
		}

		c.stopped = true
	}

	return c.selfApplyTestClient.ContainerInspect(ctx, id, opts)
}

// TestReadyAndWaitSelfApplyAcceptsDrainBeforePredecessorStop verifies that a
// predecessor stopping right after its drain acknowledgment is not mistaken
// for a lost handover.
func TestReadyAndWaitSelfApplyAcceptsDrainBeforePredecessorStop(t *testing.T) {
	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "handover", State: selfupdate.StateApplying,
		Predecessor: selfupdate.ContainerRef{ID: "old"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	fake := &drainThenStopClient{selfApplyTestClient: &selfApplyTestClient{}, store: store, recordID: record.ID}

	after, err := readyAndWaitSelfApply(t.Context(), fake, store, record)
	if err != nil || after.State != selfupdate.StateApplyDrained {
		t.Errorf("drain then stop = %s/%v; want apply_drained", after.State, err)
	}
}

// TestReadyAndWaitSelfApplyResumesAfterDrain permits a restarted applier to
// continue after the predecessor acknowledged the drain.
func TestReadyAndWaitSelfApplyResumesAfterDrain(t *testing.T) {
	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{ID: "handover", State: selfupdate.StateApplyDrained}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	after, err := readyAndWaitSelfApply(t.Context(), &selfApplyTestClient{}, store, record)
	if err != nil || after.State != selfupdate.StateApplyDrained {
		t.Errorf("resume drain = %s/%v; want apply_drained", after.State, err)
	}
}

// TestFailSelfUpdateAfterDrainRestoresPredecessor checks rollback after the
// handover passed the drain acknowledgment.
func TestFailSelfUpdateAfterDrainRestoresPredecessor(t *testing.T) {
	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "handover", State: selfupdate.StateApplyDrained,
		Predecessor: selfupdate.ContainerRef{ID: "old"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	fake := &selfApplyTestClient{stopped: true}

	err := FailSelfUpdate(context.Background(), selfApplyTestCli{apiClient: fake},
		store, record.ID, errors.New("secret provider unavailable"), nil)
	if err != nil {
		t.Fatalf("recover pre-entry failure after predecessor drain: %v", err)
	}

	after, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if after.State != selfupdate.StateRolledBack || fake.stopped ||
		after.Error != "secret provider unavailable" {
		t.Errorf("recovery after drain = %+v, predecessor stopped=%v", after, fake.stopped)
	}
}

// TestFinishSelfApplyFailureRefreshesConcurrentDrain checks rollback against
// a journal advanced while recovery was starting.
func TestFinishSelfApplyFailureRefreshesConcurrentDrain(t *testing.T) {
	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "handover", State: selfupdate.StateApplyReady,
		Predecessor: selfupdate.ContainerRef{ID: "old"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	fake := &drainDuringRecoveryClient{selfApplyTestClient: &selfApplyTestClient{}, store: store, recordID: record.ID}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := finishSelfApplyFailure(t.Context(), fake, store, record, errors.New("source reload failed"), log); err != nil {
		t.Fatalf("recover concurrent predecessor drain: %v", err)
	}

	after, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if !fake.drained || after.State != selfupdate.StateFailed || after.Error != "source reload failed" ||
		after.Restored.ID != "old" || len(after.History) != 3 {
		t.Errorf("journal after recovery/drain race = %+v; want drained then failed without losing reason", after)
	}
}

// TestApplySelfUpdateResumesPendingFailureInsteadOfApplying prevents a
// restarted clone from applying after failure recovery was requested.
func TestApplySelfUpdateResumesPendingFailureInsteadOfApplying(t *testing.T) {
	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "handover", State: selfupdate.StateApplyReady, Error: "source reload failed",
		Predecessor: selfupdate.ContainerRef{ID: "old"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	fake := &selfApplyTestClient{}
	if err := ApplySelfUpdate(t.Context(), selfApplyTestCli{apiClient: fake}, ApplySelfOptions{
		Store: store, JournalID: record.ID, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatalf("recover persisted failure: %v", err)
	}

	after, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if after.State != selfupdate.StateFailed || after.Error != record.Error || fake.started != 0 {
		t.Errorf("applier resumed work after failure intent was saved: %+v, starts=%d", after, fake.started)
	}
}
