package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/cli/cli/streams"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker/options"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

type serviceIdentityClient struct {
	client.APIClient
	services   []swarmTypes.Service
	updateErr  error
	createErr  error
	removeErr  error
	updateFunc func(context.Context, string, client.ServiceUpdateOptions) (client.ServiceUpdateResult, error)
}

func (c *serviceIdentityClient) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	return client.ServiceListResult{Items: c.services}, nil
}

func (c *serviceIdentityClient) ServiceUpdate(ctx context.Context, id string, opts client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
	if c.updateFunc != nil {
		return c.updateFunc(ctx, id, opts)
	}

	return client.ServiceUpdateResult{}, c.updateErr
}

func (c *serviceIdentityClient) ServiceCreate(context.Context, client.ServiceCreateOptions) (client.ServiceCreateResult, error) {
	return client.ServiceCreateResult{ID: "created-service-id"}, c.createErr
}

func (c *serviceIdentityClient) ServiceInspect(context.Context, string, client.ServiceInspectOptions) (client.ServiceInspectResult, error) {
	if len(c.services) == 0 {
		return client.ServiceInspectResult{}, errors.New("service not found")
	}

	return client.ServiceInspectResult{Service: c.services[0]}, nil
}

func (c *serviceIdentityClient) ServiceRemove(context.Context, string, client.ServiceRemoveOptions) (client.ServiceRemoveResult, error) {
	return client.ServiceRemoveResult{}, c.removeErr
}

type serviceIdentityCLI struct {
	command.Cli
	client client.APIClient
	out    *streams.Out
	err    *streams.Out
}

func (c serviceIdentityCLI) Client() client.APIClient { return c.client }
func (c serviceIdentityCLI) Out() *streams.Out        { return c.out }
func (c serviceIdentityCLI) Err() *streams.Out        { return c.err }

func testServiceIdentityCLI(apiClient client.APIClient, stdout, stderr io.Writer) command.Cli {
	return serviceIdentityCLI{
		client: apiClient,
		out:    streams.NewOut(stdout),
		err:    streams.NewOut(stderr),
	}
}

func assertServiceLogIdentity(t *testing.T, data []byte, name, id string) {
	t.Helper()

	decoder := json.NewDecoder(bytes.NewReader(data))
	count := 0

	for {
		var record map[string]any

		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("decode service log: %v", err)
		}

		if record["service"] != name || record["service_id"] != id {
			t.Fatalf("service identity = %v/%v, want %q/%q", record["service"], record["service_id"], name, id)
		}

		count++
	}

	if count == 0 {
		t.Fatal("expected at least one structured service log")
	}
}

func TestServiceIdentity(t *testing.T) {
	t.Parallel()

	if got := ServiceIdentity("stack_api", "service-id"); got != "stack_api (id=service-id)" {
		t.Fatalf("ServiceIdentity() = %q", got)
	}

	if got := ServiceIdentity("", "service-id"); got != "unavailable (id=service-id)" {
		t.Fatalf("ServiceIdentity() fallback = %q", got)
	}
}

func TestDeployServicesIdentity(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		existing   bool
		updateErr  error
		createErr  error
		wantID     string
		wantOutput string
	}{
		{name: "create", wantID: "created-service-id", wantOutput: "Created service"},
		{name: "update", existing: true, wantID: "existing-service-id", wantOutput: "Updating service"},
		{name: "update failure", existing: true, updateErr: errors.New("rejected"), wantID: "existing-service-id"},
		{name: "create failure", createErr: errors.New("rejected"), wantID: "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			apiClient := &serviceIdentityClient{updateErr: tc.updateErr, createErr: tc.createErr}
			if tc.existing {
				apiClient.services = []swarmTypes.Service{{ID: tc.wantID, Spec: swarmTypes.ServiceSpec{
					Name: "stack_api", TaskTemplate: swarmTypes.TaskSpec{
						ContainerSpec: &swarmTypes.ContainerSpec{Image: "example:latest"},
					},
				}}}
			}

			var stdout, stderr, logs bytes.Buffer

			cli := testServiceIdentityCLI(apiClient, &stdout, &stderr)
			log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			services := map[string]swarmTypes.ServiceSpec{
				"api": {TaskTemplate: swarmTypes.TaskSpec{ContainerSpec: &swarmTypes.ContainerSpec{Image: "example:latest"}}},
			}

			deployed, err := deployServices(t.Context(), cli, services, convert.NewNamespace("stack"),
				&options.Deploy{ResolveImage: ResolveImageNever, Logger: log})
			if tc.updateErr != nil || tc.createErr != nil {
				if err == nil || !strings.Contains(err.Error(), "stack_api (id="+tc.wantID+")") {
					t.Fatalf("deployServices() error = %v, want name and ID", err)
				}

				if tc.updateErr != nil && !errors.Is(err, tc.updateErr) || tc.createErr != nil && !errors.Is(err, tc.createErr) {
					t.Fatalf("deployServices() error = %v, want wrapped daemon error", err)
				}
			} else {
				if err != nil || len(deployed) != 1 || deployed[0].id != tc.wantID || deployed[0].name != "stack_api" {
					t.Fatalf("deployServices() = %+v, %v", deployed, err)
				}

				if !strings.Contains(stdout.String(), tc.wantOutput+" stack_api (id="+tc.wantID+")") {
					t.Fatalf("stdout = %q, want readable service name and ID", stdout.String())
				}
			}

			assertServiceLogIdentity(t, logs.Bytes(), "stack_api", tc.wantID)
		})
	}
}

func TestRemoveServicesIdentity(t *testing.T) {
	t.Parallel()

	apiClient := &serviceIdentityClient{removeErr: errors.New("daemon unavailable")}

	var stdout, stderr, logs bytes.Buffer

	cli := testServiceIdentityCLI(apiClient, &stdout, &stderr)
	log := slog.New(slog.NewJSONHandler(&logs, nil))

	if !removeServices(t.Context(), cli, []swarmTypes.Service{{ID: "full-service-id", Spec: swarmTypes.ServiceSpec{Name: "stack_api"}}}, log) {
		t.Fatal("removeServices() should report failure")
	}

	for _, output := range []string{stdout.String(), stderr.String()} {
		if !strings.Contains(output, "stack_api (id=full-service-id)") {
			t.Fatalf("service name and ID missing in %q", output)
		}
	}

	assertServiceLogIdentity(t, logs.Bytes(), "stack_api", "full-service-id")
}

func TestDeployServicesRetainsAcceptedUpdatesOnCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	spec := swarmTypes.ServiceSpec{TaskTemplate: swarmTypes.TaskSpec{
		ContainerSpec: &swarmTypes.ContainerSpec{Image: "example:latest"},
	}}
	first, second := spec, spec
	first.Name = "stack_app"
	second.Name = "stack_worker"
	calls := 0
	apiClient := &serviceIdentityClient{
		services: []swarmTypes.Service{{ID: "app-id", Spec: first}, {ID: "worker-id", Spec: second}},
		updateFunc: func(context.Context, string, client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
			calls++
			if calls == 2 {
				cancel()

				return client.ServiceUpdateResult{}, context.Canceled
			}

			return client.ServiceUpdateResult{}, nil
		},
	}

	var logs bytes.Buffer

	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cli := testServiceIdentityCLI(apiClient, io.Discard, io.Discard)

	deployed, err := deployServices(ctx, cli, map[string]swarmTypes.ServiceSpec{"app": spec, "worker": spec},
		convert.NewNamespace("stack"), &options.Deploy{ResolveImage: ResolveImageNever, Logger: log})
	if !errors.Is(err, context.Canceled) || len(deployed) != 1 {
		t.Fatalf("deployServices() = %+v, %v, want one accepted update and cancellation", deployed, err)
	}

	if !errors.Is(selfUpdateHandoverError(ctx, err, deployed[0].name, deployed), selfupdate.ErrHandover) {
		t.Fatal("accepted self update did not qualify for a handover")
	}

	if strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), "service operation canceled") {
		t.Fatalf("canceled update logged as a failure: %s", logs.String())
	}
}

func TestServiceFailureLogLevels(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		err   error
		level string
	}{
		{name: "canceled", err: fmt.Errorf("wait: %w", context.Canceled), level: "DEBUG"},
		{name: "deadline", err: context.DeadlineExceeded, level: "ERROR"},
		{name: "failure", err: errors.New("rejected"), level: "ERROR"},
		{name: "joined failure", err: errors.Join(context.Canceled, errors.New("rejected")), level: "ERROR"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logs bytes.Buffer

			log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			logServiceError(log, "failed to update service", deployedService{name: "stack_app", id: "app-id"}, tt.err)

			var record struct {
				Level string `json:"level"`
			}

			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatal(err)
			}

			if record.Level != tt.level {
				t.Fatalf("log level = %q, want %q", record.Level, tt.level)
			}

			assertServiceLogIdentity(t, logs.Bytes(), "stack_app", "app-id")
		})
	}
}

func TestDeployServicesUpdatesSelfLast(t *testing.T) {
	t.Parallel()

	spec := swarmTypes.ServiceSpec{TaskTemplate: swarmTypes.TaskSpec{
		ContainerSpec: &swarmTypes.ContainerSpec{Image: "example:latest"},
	}}
	self, worker := spec, spec
	self.Name = "stack_app"
	worker.Name = "stack_worker"

	var updates []string

	apiClient := &serviceIdentityClient{
		services: []swarmTypes.Service{{ID: "self-id", Spec: self}, {ID: "worker-id", Spec: worker}},
		updateFunc: func(_ context.Context, id string, _ client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
			updates = append(updates, id)

			return client.ServiceUpdateResult{}, nil
		},
	}
	cli := testServiceIdentityCLI(apiClient, io.Discard, io.Discard)

	deployed, err := deployServices(t.Context(), cli, map[string]swarmTypes.ServiceSpec{"app": spec, "worker": spec},
		convert.NewNamespace("stack"), &options.Deploy{ResolveImage: ResolveImageNever, SelfService: self.Name})
	if err != nil || len(deployed) != 2 {
		t.Fatalf("deployServices() = %+v, %v, want both services updated", deployed, err)
	}

	if !slices.Equal(updates, []string{"worker-id", "self-id"}) {
		t.Fatalf("service update order = %v, want worker before self", updates)
	}
}

func TestScaleServiceIdentityOnUpdateFailure(t *testing.T) {
	t.Parallel()

	updateErr := errors.New("update rejected")
	replicas := uint64(1)
	apiClient := &serviceIdentityClient{
		services: []swarmTypes.Service{{ID: "full-service-id", Spec: swarmTypes.ServiceSpec{
			Name: "stack_api", Mode: swarmTypes.ServiceMode{Replicated: &swarmTypes.ReplicatedService{Replicas: &replicas}},
		}}},
		updateErr: updateErr,
	}

	cli := testServiceIdentityCLI(apiClient, io.Discard, io.Discard)

	err := ScaleService(t.Context(), cli, "stack_api", 2, false, false)
	if !errors.Is(err, updateErr) || !strings.Contains(err.Error(), "stack_api (id=full-service-id)") {
		t.Fatalf("ScaleService() error = %v, want name, ID and wrapped daemon error", err)
	}
}

func TestDeployServicesPersistsSelfUpdateBeforeSubmittingIt(t *testing.T) {
	t.Parallel()

	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistence failure=%v", fail), func(t *testing.T) {
			t.Parallel()

			spec := swarmTypes.ServiceSpec{TaskTemplate: swarmTypes.TaskSpec{
				ContainerSpec: &swarmTypes.ContainerSpec{Image: "example:latest"},
			}}
			self, worker := spec, spec
			self.Name = "stack_app"
			worker.Name = "stack_worker"
			prepared := false
			cause := errors.New("journal unavailable")
			apiClient := &serviceIdentityClient{
				services: []swarmTypes.Service{{ID: "self-id", Spec: self}, {ID: "worker-id", Spec: worker}},
				updateFunc: func(_ context.Context, id string, opts client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
					if id == "self-id" {
						if !prepared || opts.Spec.Labels[selfupdate.SwarmHandoverLabel] != "handover-id" {
							t.Fatal("self-service update was submitted before persistence")
						}

						if opts.Spec.TaskTemplate.ContainerSpec.Labels[selfupdate.SwarmHandoverLabel] != "" {
							t.Fatal("handover marker changed the task template")
						}
					}

					return client.ServiceUpdateResult{}, nil
				},
			}
			cli := testServiceIdentityCLI(apiClient, io.Discard, io.Discard)
			deployed, err := deployServices(t.Context(), cli, map[string]swarmTypes.ServiceSpec{"app": spec, "worker": spec},
				convert.NewNamespace("stack"), &options.Deploy{
					ResolveImage: ResolveImageNever, SelfService: self.Name,
					PrepareSelfUpdate: func(_ context.Context, current swarmTypes.Service, desired *swarmTypes.ServiceSpec, waitIDs []string) error {
						if current.ID != "self-id" || !slices.Equal(waitIDs, []string{"worker-id", "self-id"}) {
							t.Fatalf("handover context = %s, %v", current.ID, waitIDs)
						}

						if fail {
							return cause
						}

						prepared = true
						desired.Labels = map[string]string{selfupdate.SwarmHandoverLabel: "handover-id"}

						return nil
					},
				})

			if fail {
				if !errors.Is(err, cause) || len(deployed) != 1 {
					t.Fatalf("persistence failure = %+v, %v", deployed, err)
				}
			} else if err != nil || len(deployed) != 2 {
				t.Fatalf("deployment = %+v, %v", deployed, err)
			}
		})
	}
}
