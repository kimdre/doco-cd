package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/compose/convert"
	"github.com/docker/cli/cli/streams"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

type serviceIdentityClient struct {
	client.APIClient
	services  []swarmTypes.Service
	updateErr error
	createErr error
	removeErr error
}

func (c *serviceIdentityClient) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	return client.ServiceListResult{Items: c.services}, nil
}

func (c *serviceIdentityClient) ServiceUpdate(context.Context, string, client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
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

			deployed, err := deployServices(t.Context(), cli, services, convert.NewNamespace("stack"), false, ResolveImageNever, log)
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
