package docker

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
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
)

func TestGetDeployStatusSwarmIdentity(t *testing.T) {
	t.Parallel()

	replicas := uint64(2)
	apiClient := &swarmPinningTestClient{services: []swarmTypes.Service{{
		ID: "full-service-id",
		Spec: swarmTypes.ServiceSpec{
			Name: "stack_api",
			Mode: swarmTypes.ServiceMode{Replicated: &swarmTypes.ReplicatedService{
				Replicas: &replicas,
			}},
		},
	}}}

	services, err := getDeployStatus(t.Context(), apiClient, true, "stack")
	if err != nil {
		t.Fatal(err)
	}

	status, ok := services[Service("api")]
	if !ok || status.Name != "stack_api" || status.ID != "full-service-id" || status.Replicas != replicas {
		t.Fatalf("deployed service identity = %+v, present=%v", status, ok)
	}
}

type swarmJobIdentityClient struct {
	client.APIClient
	name       string
	inspectErr error
	createErr  error
	removeErr  error
}

func (c swarmJobIdentityClient) ServiceInspect(context.Context, string, client.ServiceInspectOptions) (client.ServiceInspectResult, error) {
	return client.ServiceInspectResult{Service: swarmTypes.Service{Spec: swarmTypes.ServiceSpec{Name: c.name}}}, c.inspectErr
}

func (c swarmJobIdentityClient) ServiceRemove(context.Context, string, client.ServiceRemoveOptions) (client.ServiceRemoveResult, error) {
	return client.ServiceRemoveResult{}, c.removeErr
}

func (c swarmJobIdentityClient) ServiceCreate(context.Context, client.ServiceCreateOptions) (client.ServiceCreateResult, error) {
	return client.ServiceCreateResult{}, c.createErr
}

type swarmJobIdentityCLI struct {
	command.Cli
	apiClient client.APIClient
}

func (c swarmJobIdentityCLI) Client() client.APIClient { return c.apiClient }

func TestRemoveSwarmOneOffServiceIdentity(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		service    string
		inspectErr error
		wantName   string
	}{
		{name: "named service", service: "stack_backup-doco-job", wantName: "stack_backup-doco-job"},
		{name: "unavailable name", inspectErr: errors.New("not found"), wantName: "unavailable"},
		{name: "empty inspected name", wantName: "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			removeErr := errors.New("daemon unavailable")
			apiClient := swarmJobIdentityClient{name: tc.service, inspectErr: tc.inspectErr, removeErr: removeErr}

			var output bytes.Buffer

			log := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))

			err := RemoveSwarmOneOffService(t.Context(), swarmJobIdentityCLI{apiClient: apiClient}, "full-job-id", log)
			if !errors.Is(err, removeErr) || !strings.Contains(err.Error(), tc.wantName+" (id=full-job-id)") {
				t.Fatalf("RemoveSwarmOneOffService() error = %v, want identity and wrapped error", err)
			}

			decoder := json.NewDecoder(&output)
			foundError := false

			for {
				var record map[string]any
				if decodeErr := decoder.Decode(&record); errors.Is(decodeErr, io.EOF) {
					break
				} else if decodeErr != nil {
					t.Fatal(decodeErr)
				}

				if record["msg"] == "failed to remove one-off swarm service" {
					if record["service"] != tc.wantName || record["service_id"] != "full-job-id" {
						t.Fatalf("structured service identity = %v/%v", record["service"], record["service_id"])
					}

					foundError = true
				}
			}

			if !foundError {
				t.Fatalf("missing structured removal failure: %s", output.String())
			}
		})
	}
}

func TestRunSwarmJobCreateFailureIdentity(t *testing.T) {
	t.Parallel()

	createErr := errors.New("daemon unavailable")

	var output bytes.Buffer

	log := slog.New(slog.NewJSONHandler(&output, nil))
	cli := swarmJobIdentityCLI{apiClient: swarmJobIdentityClient{createErr: createErr}}
	err := RunSwarmJob(t.Context(), cli, swarm.DeployModeGlobalJob, []string{"true"}, "identity-create-failure", log)

	name := app.Name + "_identity-create-failure"
	if !errors.Is(err, createErr) || !strings.Contains(err.Error(), name+" (id=unavailable)") {
		t.Fatalf("RunSwarmJob() error = %v, want name and unavailable ID", err)
	}

	var record map[string]any
	if decodeErr := json.Unmarshal(output.Bytes(), &record); decodeErr != nil {
		t.Fatalf("decode job log: %v", decodeErr)
	}

	if record["service"] != name || record["service_id"] != "unavailable" {
		t.Fatalf("job log identity = %v/%v, want name and unavailable ID", record["service"], record["service_id"])
	}
}
