package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/selfupdate"
)

type failedScaleOutClient struct {
	client.APIClient
	containers []container.Summary
	listErr    error
	removeErr  error
	removed    []string
}

// ContainerList returns candidates for failed scale-out cleanup.
func (c *failedScaleOutClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: c.containers}, c.listErr
}

// ContainerRemove simulates a successor cleanup failure or success.
func (c *failedScaleOutClient) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	c.removed = append(c.removed, id)

	return client.ContainerRemoveResult{}, c.removeErr
}

// TestCleanupFailedScaleOutPreservesJournalForRetry keeps the journal when
// cleanup cannot confirm the successor was removed.
func TestCleanupFailedScaleOutPreservesJournalForRetry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   selfupdate.State
		known   bool
		listErr error
	}{
		{name: "started successor known", state: selfupdate.StateStarted, known: true},
		{name: "partially created successor discovered", state: selfupdate.StateStaged},
		{name: "candidate list unavailable", state: selfupdate.StateStaged, listErr: errors.New("daemon unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := selfupdate.NewStore(t.TempDir())

			record := selfupdate.Record{
				ID: "scale-out", State: tc.state, Strategy: selfupdate.StrategyScaleOut,
				Stack: "self-stack", Service: "app",
				Predecessor: selfupdate.ContainerRef{ID: "old"},
			}
			if tc.known {
				record.Successor = selfupdate.ContainerRef{ID: "new", Name: "self-stack-app-2", Number: 2}
			}

			if err := store.Create(&record); err != nil {
				t.Fatal(err)
			}

			fake := &failedScaleOutClient{
				containers: []container.Summary{
					{ID: "old"},
					{ID: "new", Names: []string{"/self-stack-app-2"}, Labels: map[string]string{api.ContainerNumberLabel: "2"}},
				},
				listErr: tc.listErr, removeErr: errors.New("Docker remove unavailable"),
			}

			preserve, err := cleanupFailedScaleOut(t.Context(), fake, store, &record,
				&selfTarget{Project: "self-stack", Service: "app"}, errors.New("successor failed health"))
			if !preserve || err == nil {
				t.Fatalf("cleanup = preserve %t, error %v; want retryable failure", preserve, err)
			}

			after, loadErr := store.Load(record.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}

			active, activeErr := store.Active()
			if activeErr != nil || active == nil || active.ID != record.ID {
				t.Fatalf("active recovery journal = %+v (%v); want %s", active, activeErr, record.ID)
			}

			if after.State != selfupdate.StateAborted ||
				!strings.Contains(after.Error, "successor failed health") {
				t.Errorf("recovery record = %s/%q; want aborted with failure reason", after.State, after.Error)
			}

			if tc.listErr == nil {
				if after.Successor.ID != "new" || after.Successor.Number != 2 ||
					!strings.Contains(after.Error, "Docker remove unavailable") ||
					len(fake.removed) != selfupdate.RecoveryRetries+1 {
					t.Errorf("candidate cleanup = %+v, removals=%v, reason=%q", after.Successor, fake.removed, after.Error)
				}
			} else if len(fake.removed) != 0 || !strings.Contains(after.Error, tc.listErr.Error()) {
				t.Errorf("unknown candidate cleanup = %v, reason=%q", fake.removed, after.Error)
			}
		})
	}
}

// TestCleanupFailedScaleOutRemovesOrConfirmsNoCandidate checks that cleanup
// completes only after removing or ruling out a replacement.
func TestCleanupFailedScaleOutRemovesOrConfirmsNoCandidate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		containers []container.Summary
		known      bool
	}{
		{name: "candidate removed", known: true},
		{name: "nothing was created", containers: []container.Summary{{ID: "old"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := selfupdate.NewStore(t.TempDir())

			record := selfupdate.Record{
				ID: "scale-out", State: selfupdate.StateStaged, Strategy: selfupdate.StrategyScaleOut,
				Predecessor: selfupdate.ContainerRef{ID: "old"},
			}
			if tc.known {
				record.Successor.ID = "new"
			}

			if err := store.Create(&record); err != nil {
				t.Fatal(err)
			}

			fake := &failedScaleOutClient{containers: tc.containers}

			preserve, err := cleanupFailedScaleOut(t.Context(), fake, store, &record,
				&selfTarget{Project: "self-stack", Service: "app"}, errors.New("scale-out failed"))
			if preserve || err == nil || err.Error() != "scale-out failed" {
				t.Fatalf("cleanup = preserve %t, error %v; want original error and no leftover", preserve, err)
			}

			if tc.known && (len(fake.removed) != 1 || fake.removed[0] != "new") {
				t.Errorf("removed = %v; want new", fake.removed)
			}
		})
	}
}
