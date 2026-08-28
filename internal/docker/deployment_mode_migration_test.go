package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

type deploymentModeMigrationClient struct {
	client.APIClient
	containers []container.Summary
}

func (c deploymentModeMigrationClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: c.containers}, nil
}

type deploymentModeMigrationCLI struct {
	command.Cli
	apiClient client.APIClient
}

func (c deploymentModeMigrationCLI) Client() client.APIClient {
	return c.apiClient
}

func TestMigrateDeploymentMode_RejectsUnmanagedPreviousMode(t *testing.T) {
	t.Parallel()

	dockerCli := deploymentModeMigrationCLI{apiClient: deploymentModeMigrationClient{
		containers: []container.Summary{{
			Names: []string{"/example_web_1"},
			Labels: map[string]string{
				api.ProjectLabel: "example",
			},
		}},
	}}

	err := MigrateDeploymentMode(t.Context(), nil, dockerCli, "", "example", "github.com/acme/example", true, true)
	if !errors.Is(err, ErrDeploymentModeConflict) {
		t.Fatalf("MigrateDeploymentMode() error = %v, want ErrDeploymentModeConflict", err)
	}
}
