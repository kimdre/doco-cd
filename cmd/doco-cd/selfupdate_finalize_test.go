package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

func TestResolveSelfUpdateRole(t *testing.T) {
	t.Parallel()

	record := selfupdate.Record{
		Predecessor: selfupdate.ContainerRef{ID: "old"},
		Successor:   selfupdate.ContainerRef{ID: "new"},
	}

	restored := selfupdate.Record{
		Predecessor: selfupdate.ContainerRef{ID: "old"},
		Restored:    selfupdate.ContainerRef{ID: "restored"},
	}

	tests := []struct {
		name   string
		record selfupdate.Record
		ownID  string
		want   selfUpdateRole
	}{
		{name: "predecessor", record: record, ownID: "old", want: rolePredecessor},
		{name: "successor", record: record, ownID: "new", want: roleSuccessor},
		{name: "restored predecessor", record: restored, ownID: "restored", want: rolePredecessor},
		// The applier creates the successor, so the record does not name it.
		{name: "unnamed successor", record: record, ownID: "created-by-the-applier", want: roleSuccessor},
		{name: "no container id", record: record, want: roleUnknown},
		{name: "record without a predecessor", record: selfupdate.Record{}, ownID: "whatever", want: roleUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := resolveSelfUpdateRole(tt.record, tt.ownID); got != tt.want {
				t.Errorf("role = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestWaitForDrainDoesNotTreatTimeoutAsHandover verifies that a timeout cannot
// authorize takeover before the predecessor records its drain.
func TestWaitForDrainDoesNotTreatTimeoutAsHandover(t *testing.T) {
	t.Parallel()

	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{ID: "slow-drain", State: selfupdate.StateStarted}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	_, ready, err := waitForDrain(ctx, logger.New(slog.LevelError), store, record)
	if !errors.Is(err, context.DeadlineExceeded) || ready {
		t.Fatalf("waitForDrain() = ready %v, err %v; want deadline exceeded without takeover", ready, err)
	}

	current, err := store.Load(record.ID)
	if err != nil || current.State != selfupdate.StateStarted {
		t.Fatalf("journal state = %s, err %v; want started", current.State, err)
	}
}

// TestWaitForDrainReadsCurrentJournal checks the persisted handover state
// rather than trusting the caller's initial record.
func TestWaitForDrainReadsCurrentJournal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		advance bool
		remove  bool
		ready   bool
		state   selfupdate.State
	}{
		{name: "drained", advance: true, ready: true, state: selfupdate.StateDrained},
		{name: "aborted", state: selfupdate.StateAborted},
		{name: "record removed", remove: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := selfupdate.NewStore(t.TempDir())

			record := selfupdate.Record{ID: "handover", State: selfupdate.StateStarted}
			if err := store.Create(&record); err != nil {
				t.Fatal(err)
			}

			if tt.advance {
				updated, err := store.Update(record, selfupdate.StateHandover, selfupdate.ActorPredecessor)
				if err != nil {
					t.Fatal(err)
				}

				if _, err = store.Update(updated, selfupdate.StateDrained, selfupdate.ActorPredecessor); err != nil {
					t.Fatal(err)
				}
			} else if tt.remove {
				if err := store.Remove(record.ID); err != nil {
					t.Fatal(err)
				}
			} else if _, err := store.Update(record, selfupdate.StateAborted, selfupdate.ActorPredecessor); err != nil {
				t.Fatal(err)
			}

			current, ready, err := waitForDrain(t.Context(), logger.New(slog.LevelError), store, record)
			if err != nil || ready != tt.ready || current.State != tt.state {
				t.Fatalf("waitForDrain() = %s, %v, %v; want %s, %v, nil", current.State, ready, err, tt.state, tt.ready)
			}
		})
	}
}

type finalizerDockerClient struct {
	client.APIClient
	stopErr    error
	removeErr  error
	inspectErr error
	unhealthy  bool
	stops      int
	removes    int
	removedIDs []string
	list       []container.Summary
	listErr    error
}

// ContainerStop records stop attempts for the finalizer tests.
func (c *finalizerDockerClient) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	c.stops++

	return client.ContainerStopResult{}, c.stopErr
}

// ContainerRemove records removal attempts and returns the configured error.
func (c *finalizerDockerClient) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	c.removes++
	c.removedIDs = append(c.removedIDs, id)

	return client.ContainerRemoveResult{}, c.removeErr
}

// ContainerList exposes configured containers or a listing error.
func (c *finalizerDockerClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: c.list}, c.listErr
}

// ContainerInspect returns the configured successor state for recovery tests.
func (c *finalizerDockerClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if c.inspectErr != nil {
		return client.ContainerInspectResult{}, c.inspectErr
	}

	state := &container.State{Running: true}
	if c.unhealthy {
		state.Health = &container.Health{Status: container.Unhealthy}
	}

	return client.ContainerInspectResult{Container: container.InspectResponse{State: state}}, nil
}

// TestRemoveSelfUpdateLeftoversPreservesFailedCleanup keeps cleanup failures
// recoverable instead of discarding their journal records.
func TestRemoveSelfUpdateLeftoversPreservesFailedCleanup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		stopErr    error
		removeErr  error
		wantErr    bool
		wantRemove bool
	}{
		{name: "stop fails", stopErr: errdefs.ErrUnavailable, wantErr: true},
		{name: "remove fails", removeErr: errdefs.ErrUnavailable, wantErr: true, wantRemove: true},
		{name: "already gone", stopErr: errdefs.ErrNotFound, removeErr: errdefs.ErrNotFound, wantRemove: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			apiClient := &finalizerDockerClient{stopErr: tt.stopErr, removeErr: tt.removeErr}
			record := selfupdate.Record{Predecessor: selfupdate.ContainerRef{ID: "old"}, Stack: "self"}

			err := removeSelfUpdateLeftovers(t.Context(), logger.New(slog.LevelError), apiClient, record)
			if (err != nil) != tt.wantErr {
				t.Fatalf("removeSelfUpdateLeftovers() error = %v; wantErr %v", err, tt.wantErr)
			}

			if got := apiClient.removes > 0; got != tt.wantRemove {
				t.Errorf("removed predecessor = %v, want %v", got, tt.wantRemove)
			}
		})
	}
}

// TestWaitForApplierDoesNotTreatTimeoutAsCompletion prevents a timed-out
// readiness wait from being mistaken for a completed apply.
func TestWaitForApplierDoesNotTreatTimeoutAsCompletion(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	apiClient := &finalizerDockerClient{}

	record := selfupdate.Record{ID: "applying", Applier: selfupdate.ContainerRef{ID: "clone"}}
	if err := waitForApplier(ctx, logger.New(slog.LevelError), apiClient, record); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForApplier() error = %v, want context deadline exceeded", err)
	}

	apiClient.inspectErr = errdefs.ErrUnavailable
	if err := waitForApplier(t.Context(), logger.New(slog.LevelError), apiClient, record); !errors.Is(err, errdefs.ErrUnavailable) {
		t.Fatalf("waitForApplier() error = %v, want Docker error", err)
	}
}

// TestPredecessorRestartRollsBackUnhealthyStartedSuccessor checks recovery
// after a successor starts but fails its health gate.
func TestPredecessorRestartRollsBackUnhealthyStartedSuccessor(t *testing.T) {
	previous := docker.SelfUpdateConfig()

	docker.ConfigureSelfUpdate(docker.SelfUpdateOptions{
		Identity: selfupdate.Identity{ContainerID: "old"},
	})
	t.Cleanup(func() { docker.ConfigureSelfUpdate(previous) })

	store := selfupdate.NewStore(t.TempDir())
	record := selfupdate.Record{
		ID:          "started",
		State:       selfupdate.StateStarted,
		Stack:       "self",
		Predecessor: selfupdate.ContainerRef{ID: "old"},
		Successor:   selfupdate.ContainerRef{ID: "unhealthy"},
		Source:      selfupdate.SourceInfo{CommitSHA: "new", ProjectHash: "changed"},
	}

	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	apiClient := &finalizerDockerClient{unhealthy: true}
	if err := finalizeAsPredecessor(t.Context(), logger.New(slog.LevelError), apiClient, nil, store, record); err != nil {
		t.Fatal(err)
	}

	if apiClient.stops != 0 {
		t.Errorf("stopped predecessor %d times; want 0", apiClient.stops)
	}

	for _, id := range apiClient.removedIDs {
		if id != "unhealthy" {
			t.Errorf("removed %s instead of the unhealthy successor", id)
		}
	}

	if _, err := store.Load(record.ID); !errors.Is(err, selfupdate.ErrNoRecord) {
		t.Errorf("record still active after rollback: %v", err)
	}

	if _, poisoned, err := store.IsPoisoned("", "self", "new", "changed"); err != nil || !poisoned {
		t.Errorf("failed revision not poisoned: poisoned=%v, err=%v", poisoned, err)
	}
}

// TestRemoveAbortedSuccessorDiscoversUnrecordedCandidate checks cleanup when
// the replacement container ID was never journaled.
func TestRemoveAbortedSuccessorDiscoversUnrecordedCandidate(t *testing.T) {
	t.Parallel()

	record := selfupdate.Record{
		Stack:       "self",
		Service:     "doco-cd",
		Predecessor: selfupdate.ContainerRef{ID: "old"},
	}

	apiClient := &finalizerDockerClient{list: []container.Summary{{ID: "old"}, {ID: "candidate"}}}
	if err := removeAbortedSuccessor(t.Context(), apiClient, record); err != nil {
		t.Fatal(err)
	}

	if len(apiClient.removedIDs) != 1 || apiClient.removedIDs[0] != "candidate" {
		t.Errorf("removed IDs = %v; want only the unrecorded candidate", apiClient.removedIDs)
	}

	apiClient.list = append(apiClient.list, container.Summary{ID: "another"})

	apiClient.removedIDs = nil
	if err := removeAbortedSuccessor(t.Context(), apiClient, record); err == nil {
		t.Fatal("ambiguous successor list must not remove an arbitrary container")
	}

	if len(apiClient.removedIDs) != 0 {
		t.Errorf("removed candidates despite ambiguity: %v", apiClient.removedIDs)
	}
}
