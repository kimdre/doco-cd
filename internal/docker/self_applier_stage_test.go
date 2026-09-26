package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/selfupdate"
)

type stagingApplierClient struct {
	client.APIClient
	source      *container.InspectResponse
	created     client.ContainerCreateOptions
	removeErr   error
	startErr    error
	removeCalls int
	startCalls  int
	connections []string
	connectErr  error
}

// ContainerInspect supplies the source container for clone staging tests.
func (c *stagingApplierClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if c.source != nil {
		return client.ContainerInspectResult{Container: *c.source}, nil
	}

	return client.ContainerInspectResult{Container: applierSourceInspect()}, nil
}

// ContainerCreate records the clone options and returns the mock clone ID.
func (c *stagingApplierClient) ContainerCreate(_ context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	c.created = opts

	return client.ContainerCreateResult{ID: "clone"}, nil
}

// ContainerStart counts attempts and optionally fails clone startup.
func (c *stagingApplierClient) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	c.startCalls++
	return client.ContainerStartResult{}, c.startErr
}

// NetworkConnect checks that a staged clone does not inherit service aliases.
func (c *stagingApplierClient) NetworkConnect(_ context.Context, networkID string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	if options.Container != "clone" || options.EndpointConfig != nil {
		return client.NetworkConnectResult{}, errors.New("applier inherited predecessor network identity")
	}

	c.connections = append(c.connections, networkID)

	return client.NetworkConnectResult{}, c.connectErr
}

// ContainerRemove counts attempts and optionally fails clone cleanup.
func (c *stagingApplierClient) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	c.removeCalls++

	return client.ContainerRemoveResult{}, c.removeErr
}

type stagingApplierJournal struct {
	store     *selfupdate.Store
	saveErr   error
	updateErr error
}

// Save injects a journal failure or persists the staged record.
func (j stagingApplierJournal) Save(record selfupdate.Record) error {
	if j.saveErr != nil {
		return j.saveErr
	}

	return j.store.Save(record)
}

// Update injects a journal transition failure or forwards it to the store.
func (j stagingApplierJournal) Update(record selfupdate.Record, to selfupdate.State, actor selfupdate.Actor) (selfupdate.Record, error) {
	if j.updateErr != nil {
		return record, j.updateErr
	}

	return j.store.Update(record, to, actor)
}

// TestFailedApplierStageCleansOrRetainsClone checks cleanup and retained
// journal state for failures at each clone staging step.
func TestFailedApplierStageCleansOrRetainsClone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failStep   string
		removeFail bool
		wantState  selfupdate.State
	}{
		{name: "Save failure clone removed", failStep: "save"},
		{name: "Update failure clone removed", failStep: "update"},
		{name: "Start failure clone removed", failStep: "start"},
		{name: "Connect failure clone removed", failStep: "connect"},
		{name: "Update failure removal retained", failStep: "update", removeFail: true, wantState: selfupdate.StateAborted},
		{name: "Start failure removal retained", failStep: "start", removeFail: true, wantState: selfupdate.StateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := selfupdate.NewStore(t.TempDir())

			record := selfupdate.Record{
				ID: "staging", State: selfupdate.StateStaged, Strategy: selfupdate.StrategyApplier,
				Stack: "self-stack", Predecessor: selfupdate.ContainerRef{ID: "old"},
				Deploy: selfupdate.DeployInfo{NetworkDrift: true},
			}
			if err := store.Create(&record); err != nil {
				t.Fatal(err)
			}

			fake := &stagingApplierClient{}

			journal := stagingApplierJournal{store: store}
			if tc.failStep == "save" {
				journal.saveErr = errors.New("save journal unavailable")
			}

			if tc.failStep == "update" {
				journal.updateErr = errors.New("update journal unavailable")
			}

			if tc.failStep == "start" {
				fake.startErr = errors.New("start clone unavailable")
			}

			if tc.failStep == "connect" {
				fake.connectErr = errors.New("connect clone unavailable")
			}

			if tc.removeFail {
				fake.removeErr = errors.New("remove clone unavailable")
			}

			log := slog.New(slog.NewTextHandler(io.Discard, nil))

			err := stageSelfApplier(t.Context(), fake, journal, "old", "", &record, log)
			if err == nil || record.Applier.ID != "clone" {
				t.Fatalf("stage = %+v, %v; want clone ID retained on failure", record.Applier, err)
			}

			if tc.failStep != "start" && fake.startCalls != 0 {
				t.Errorf("started clone despite persistence failure: %d calls", fake.startCalls)
			}

			if len(fake.connections) != 1 || fake.connections[0] != "doco-cd_default" {
				t.Errorf("clone preflight networks = %v; want predecessor network before start", fake.connections)
			}

			preserve, cleanupErr := cleanupFailedApplierStage(t.Context(), fake, store, &record, err)
			if preserve != tc.removeFail || cleanupErr == nil || fake.removeCalls == 0 {
				t.Fatalf("cleanup = preserve %t, error %v, removals %d; want preserved=%t",
					preserve, cleanupErr, fake.removeCalls, tc.removeFail)
			}

			if tc.removeFail {
				after, loadErr := store.Load(record.ID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}

				active, activeErr := store.Active()
				if activeErr != nil || active == nil || active.ID != record.ID {
					t.Fatalf("recovery journal not retained: %+v (%v)", active, activeErr)
				}

				if after.State != tc.wantState || after.Applier.ID != "clone" ||
					!strings.Contains(after.Error, "remove clone unavailable") ||
					!strings.Contains(after.Error, tc.failStep) {
					t.Errorf("persisted cleanup failure = %+v; want %s with both errors", after, tc.wantState)
				}

				return
			}

			if err := store.RemoveIfUnchanged(record); err != nil {
				t.Fatal(err)
			}

			if active, err := store.Active(); err != nil || active != nil {
				t.Errorf("orphan journal after successful cleanup: %+v (%v)", active, err)
			}
		})
	}
}

// TestStageSelfApplierNetworks checks that without drift the clone keeps the
// predecessor's network mode and joins only its other networks, while drift
// moves it to the bridge and joins every predecessor network.
func TestStageSelfApplierNetworks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name            string
		mode            container.NetworkMode
		drift           bool
		wantMode        container.NetworkMode
		wantConnections []string
	}{
		{name: "mode by name", mode: "doco-cd_default", wantMode: "doco-cd_default", wantConnections: []string{"backend-id"}},
		{name: "mode by ID", mode: "default-id", wantMode: "default-id", wantConnections: []string{"backend-id"}},
		{name: "drift", mode: "doco-cd_default", drift: true, wantMode: "bridge", wantConnections: []string{"backend-id", "default-id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			source := applierSourceInspect()
			source.HostConfig.NetworkMode = tc.mode
			source.NetworkSettings.Networks = map[string]*network.EndpointSettings{
				"doco-cd_default": {NetworkID: "default-id", Aliases: []string{"app"}},
				"doco-cd_backend": {NetworkID: "backend-id", Aliases: []string{"app"}},
			}

			store := selfupdate.NewStore(t.TempDir())

			record := selfupdate.Record{
				ID: "staging", State: selfupdate.StateStaged, Strategy: selfupdate.StrategyApplier,
				Stack: "self-stack", Predecessor: selfupdate.ContainerRef{ID: "old"},
				Deploy: selfupdate.DeployInfo{NetworkDrift: tc.drift},
			}
			if err := store.Create(&record); err != nil {
				t.Fatal(err)
			}

			fake := &stagingApplierClient{source: &source}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))

			if err := stageSelfApplier(t.Context(), fake, store, "old", "", &record, log); err != nil {
				t.Fatal(err)
			}

			if got := fake.created.HostConfig.NetworkMode; got != tc.wantMode {
				t.Errorf("clone network mode = %q, want %q", got, tc.wantMode)
			}

			if !slices.Equal(fake.connections, tc.wantConnections) {
				t.Errorf("clone connections = %v, want %v", fake.connections, tc.wantConnections)
			}
		})
	}
}

// TestFailedApplierStageDoesNotRemoveAdvancedJournal prevents staging cleanup
// from deleting a record advanced by another process.
func TestFailedApplierStageDoesNotRemoveAdvancedJournal(t *testing.T) {
	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{ID: "staging", State: selfupdate.StateStaged, Strategy: selfupdate.StrategyApplier}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	fake := &stagingApplierClient{}
	journal := stagingApplierJournal{store: store, updateErr: errors.New("failed to mark applying")}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	stageErr := stageSelfApplier(t.Context(), fake, journal, "old", "", &record, log)
	if stageErr == nil {
		t.Fatal("expected stage error")
	}

	other, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = store.Update(other, selfupdate.StateApplying, selfupdate.ActorPredecessor); err != nil {
		t.Fatal(err)
	}

	preserve, cleanupErr := cleanupFailedApplierStage(t.Context(), fake, store, &record, stageErr)
	if !preserve || !errors.Is(cleanupErr, selfupdate.ErrStaleRecord) || fake.removeCalls != 1 {
		t.Fatalf("advanced journal cleanup = preserve %t, error %v, removals %d", preserve, cleanupErr, fake.removeCalls)
	}

	active, err := store.Active()
	if err != nil || active == nil || active.State != selfupdate.StateApplying {
		t.Errorf("advanced journal was lost: %+v (%v)", active, err)
	}
}
