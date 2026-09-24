package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/containerd/errdefs"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

type selfUpdateVolumeClient struct {
	client.APIClient
	volumes      []volume.Volume
	inspected    map[string]volume.Volume
	inspectCalls int
}

// VolumeList returns existing project volumes to the validation helper.
func (c *selfUpdateVolumeClient) VolumeList(context.Context, client.VolumeListOptions) (client.VolumeListResult, error) {
	return client.VolumeListResult{Items: c.volumes}, nil
}

// VolumeInspect returns a named volume or the configured inspect error.
func (c *selfUpdateVolumeClient) VolumeInspect(_ context.Context, name string, _ client.VolumeInspectOptions) (client.VolumeInspectResult, error) {
	c.inspectCalls++

	found, ok := c.inspected[name]
	if !ok {
		return client.VolumeInspectResult{}, errdefs.ErrNotFound
	}

	return client.VolumeInspectResult{Volume: found}, nil
}

// TestValidateSelfUpdateVolumesRejectsDataReplacement rejects changes that
// could destroy existing volume data during self-update.
func TestValidateSelfUpdateVolumesRejectsDataReplacement(t *testing.T) {
	desired := types.VolumeConfig{Name: "stack_data", Driver: "local", Labels: types.Labels{"purpose": "data"}}

	hash, err := compose.VolumeHash(desired)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		desired   types.VolumeConfig
		existing  volume.Volume
		wantError bool
	}{
		{name: "unchanged volume", desired: desired, existing: volume.Volume{
			Name: desired.Name, Driver: "local",
			Labels: map[string]string{api.ProjectLabel: "stack", api.VolumeLabel: "data", api.ConfigHashLabel: hash, "purpose": "data"},
		}},
		{name: "changed Compose hash", desired: desired, existing: volume.Volume{
			Name: desired.Name, Driver: "local",
			Labels: map[string]string{api.ProjectLabel: "stack", api.VolumeLabel: "data", api.ConfigHashLabel: "old", "purpose": "data"},
		}, wantError: true},
		{name: "changed options without hash", desired: desired, existing: volume.Volume{
			Name: desired.Name, Driver: "local", Options: map[string]string{"type": "tmpfs"},
			Labels: map[string]string{api.ProjectLabel: "stack", api.VolumeLabel: "data", "purpose": "data"},
		}, wantError: true},
		{name: "changed user labels", desired: desired, existing: volume.Volume{
			Name: desired.Name, Driver: "local",
			Labels: map[string]string{api.ProjectLabel: "stack", api.VolumeLabel: "data", "purpose": "old"},
		}, wantError: true},
		{
			name: "renamed volume", desired: types.VolumeConfig{Name: "stack_data_new", Driver: "local"},
			existing: volume.Volume{Name: "stack_data", Driver: "local", Labels: map[string]string{
				api.ProjectLabel: "stack", api.VolumeLabel: "data",
			}}, wantError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := &types.Project{Name: "stack", Volumes: types.Volumes{"data": tc.desired}}
			fake := &selfUpdateVolumeClient{
				volumes: []volume.Volume{tc.existing}, inspected: map[string]volume.Volume{tc.existing.Name: tc.existing},
			}

			err := validateSelfUpdateVolumes(t.Context(), fake, project)
			if tc.wantError {
				if !errors.Is(err, selfupdate.ErrUnsupported) || !strings.Contains(err.Error(), "volume data cannot be restored") {
					t.Errorf("volume validation = %v; want explicit unsupported volume replacement", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestValidateSelfUpdateVolumesPermitsNewAndExternalVolumes accepts volumes
// that Compose will not replace during the update.
func TestValidateSelfUpdateVolumesPermitsNewAndExternalVolumes(t *testing.T) {
	project := &types.Project{Name: "stack", Volumes: types.Volumes{
		"new":      {Name: "stack_new"},
		"external": {Name: "shared-data", External: true},
	}}

	fake := &selfUpdateVolumeClient{}
	if err := validateSelfUpdateVolumes(t.Context(), fake, project); err != nil {
		t.Fatal(err)
	}

	if fake.inspectCalls != 1 {
		t.Errorf("inspected %d volumes; want only the new managed volume", fake.inspectCalls)
	}
}

type selfUpdateVolumePreflightClient struct {
	driftNetworkClient
	volumes selfUpdateVolumeClient
}

// VolumeList records the preflight volume listing before clone staging.
func (c *selfUpdateVolumePreflightClient) VolumeList(ctx context.Context, opts client.VolumeListOptions) (client.VolumeListResult, error) {
	return c.volumes.VolumeList(ctx, opts)
}

// VolumeInspect records preflight inspection of an existing volume.
func (c *selfUpdateVolumePreflightClient) VolumeInspect(ctx context.Context, name string, opts client.VolumeInspectOptions) (client.VolumeInspectResult, error) {
	return c.volumes.VolumeInspect(ctx, name, opts)
}

// TestSelfUpdateVolumeValidationBeforeStaging ensures unsafe volume changes
// are rejected before an applier clone is created.
func TestSelfUpdateVolumeValidationBeforeStaging(t *testing.T) {
	for _, tc := range []struct {
		name     string
		drift    bool
		strategy selfupdate.Strategy
	}{
		{name: "scale-out without network drift"},
		{name: "applier without network drift", strategy: selfupdate.StrategyApplier},
		{name: "applier with network drift", drift: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := selfUpdateTestProject("stack", "app")
			if tc.drift {
				svc := project.Services["app"]
				svc.Networks = map[string]*types.ServiceNetworkConfig{"backend": {}}
				project.Services["app"] = svc
				project.Networks = types.Networks{"backend": {Name: "stack_backend"}}
			}

			project.Volumes = types.Volumes{"data": {Name: "stack_data", Driver: "local"}}
			fake := &selfUpdateVolumePreflightClient{
				network: network.Inspect{Network: network.Network{
					ID: "network", Name: "stack_backend",
					Labels: map[string]string{api.ProjectLabel: "stack", api.NetworkLabel: "backend", api.ConfigHashLabel: "old"},
				}},
				volumes: selfUpdateVolumeClient{volumes: []volume.Volume{{
					Name: "stack_data", Driver: "local", Options: map[string]string{"type": "nfs"},
					Labels: map[string]string{api.ProjectLabel: "stack", api.VolumeLabel: "data"},
				}}},
			}

			withSelfIdentity(t, "stack", "app")

			opts := SelfUpdateConfig()
			opts.Store = selfupdate.NewStore(t.TempDir())
			opts.Strategy = tc.strategy
			ConfigureSelfUpdate(opts)

			_, _, step, deferred, err := prepareSelfUpdate(t.Context(), selfApplyTestCli{apiClient: fake}, project,
				&deploy.Config{}, &selfTarget{Project: "stack", Service: "app"}, nil, &SelfDeployInput{SourceType: "git"})
			if !errors.Is(err, selfupdate.ErrUnsupported) || step != nil || deferred {
				t.Fatalf("preflight = step %v, deferred %t, error %v; want refusal before staging", step != nil, deferred, err)
			}
		})
	}
}

// TestSelfUpdateComposeCreatesRejectLateVolumeChange rechecks volume safety
// immediately before Compose creates the self service.
func TestSelfUpdateComposeCreatesRejectLateVolumeChange(t *testing.T) {
	project := selfUpdateTestProject("stack", "app")
	project.Volumes = types.Volumes{"data": {Name: "stack_data", Driver: "local"}}

	fake := &selfUpdateVolumeClient{volumes: []volume.Volume{{
		Name: "stack_data", Driver: "local",
		Labels: map[string]string{api.ProjectLabel: "stack", api.VolumeLabel: "data"},
	}}}
	if err := validateSelfUpdateVolumes(t.Context(), fake, project); err != nil {
		t.Fatalf("unchanged volume failed early preflight: %v", err)
	}
	// Simulate volume drift after source pull/build or while the predecessor
	// drains. Each Create entry point must independently refuse the new state.
	fake.volumes[0].Options = map[string]string{"type": "nfs"}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := applySelfService(t.Context(), fake, nil, project,
		selfupdate.Record{Service: "app"}, nil, log); !errors.Is(err, selfupdate.ErrUnsupported) {
		t.Errorf("applier Compose Create with changed volume = %v; want refusal", err)
	}

	if err := selfUpdateScaleOut(t.Context(), selfApplyTestCli{apiClient: fake}, project,
		&deploy.Config{}, &selfTarget{Project: "stack", Service: "app"}, &selfupdate.Record{}, log); !errors.Is(err, selfupdate.ErrUnsupported) {
		t.Errorf("scale-out Compose Create with changed volume = %v; want refusal", err)
	}
}

type volumeChangesAfterPreflightClient struct {
	*selfUpdateVolumeClient
	checks int
}

// VolumeList simulates a volume changing after initial validation.
func (c *volumeChangesAfterPreflightClient) VolumeList(ctx context.Context, opts client.VolumeListOptions) (client.VolumeListResult, error) {
	c.checks++
	if c.checks == 2 {
		c.volumes[0].Options = map[string]string{"type": "nfs"}
	}

	return c.selfUpdateVolumeClient.VolumeList(ctx, opts)
}

// TestApplySelfDriftProjectRechecksVolumeImmediatelyBeforeCreate checks
// volume safety again at the network-drift Create boundary.
func TestApplySelfDriftProjectRechecksVolumeImmediatelyBeforeCreate(t *testing.T) {
	project := selfUpdateTestProject("stack", "app")
	project.Volumes = types.Volumes{"data": {Name: "stack_data", Driver: "local"}}
	fake := &volumeChangesAfterPreflightClient{selfUpdateVolumeClient: &selfUpdateVolumeClient{
		volumes: []volume.Volume{{
			Name: "stack_data", Driver: "local",
			Labels: map[string]string{api.ProjectLabel: "stack", api.VolumeLabel: "data"},
		}},
	}}
	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "drift", State: selfupdate.StateApplyDrained, Stack: "stack", Service: "app",
		Drift: &selfupdate.DriftSnapshot{Networks: map[string]network.Inspect{}},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	err := applySelfDriftProject(t.Context(), selfApplyTestCli{apiClient: fake}, fake, nil,
		project, &record, store, nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, selfupdate.ErrUnsupported) || fake.checks != 2 {
		t.Fatalf("late change before Compose Create = %v after %d checks; want refusal on second check", err, fake.checks)
	}
}

// TestApplySelfDriftProjectRejectsLateVolumeChange prevents a volume
// replacement discovered after the initial drift preflight.
func TestApplySelfDriftProjectRejectsLateVolumeChange(t *testing.T) {
	project := selfUpdateTestProject("stack", "app")
	project.Volumes = types.Volumes{"data": {Name: "stack_data", Driver: "local"}}
	fake := &selfUpdateVolumeClient{volumes: []volume.Volume{{
		Name: "stack_data", Driver: "local", Options: map[string]string{"type": "nfs"},
		Labels: map[string]string{api.ProjectLabel: "stack", api.VolumeLabel: "data"},
	}}}
	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "drift", State: selfupdate.StateApplyDrained,
		Drift: &selfupdate.DriftSnapshot{Networks: map[string]network.Inspect{}},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	err := applySelfDriftProject(t.Context(), selfApplyTestCli{apiClient: fake}, fake, nil,
		project, &record, store, nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, selfupdate.ErrUnsupported) {
		t.Fatalf("apply with changed existing volume = %v; want refusal before Compose Create", err)
	}

	if record.DriftStarted {
		t.Fatal("marked drift started before volume validation")
	}
}
