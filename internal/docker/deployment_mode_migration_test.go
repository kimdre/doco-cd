package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	containerTypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/test"
)

type deploymentModeMigrationClient struct {
	client.APIClient
	containers []containerTypes.Summary
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
		containers: []containerTypes.Summary{{
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

func TestMigrateDeploymentMode_PreservesNamedVolumes(t *testing.T) {
	dockerCli, err := CreateDockerCli(false)
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}

	ctx := t.Context()
	apiClient := dockerCli.Client()

	swarmAvailable, err := swarm.ResolveModeEnabled(ctx, apiClient)
	if err != nil {
		t.Skipf("failed to determine swarm availability: %v", err)
	}

	if !swarmAvailable {
		t.Skip("Swarm mode is not enabled, skipping migration integration test")
	}

	imageReader, err := apiClient.ImagePull(ctx, "busybox:latest", client.ImagePullOptions{})
	if err != nil {
		t.Skipf("cannot pull busybox test image: %v", err)
	}
	defer imageReader.Close()

	if _, err := io.Copy(io.Discard, imageReader); err != nil {
		t.Fatalf("failed to pull busybox test image: %v", err)
	}

	for _, tt := range []struct {
		name       string
		sourceMode bool
		create     func(string, string) (func(), error)
		assertGone func(string) error
	}{
		{
			name:       "compose to swarm",
			sourceMode: false,
			create: func(stackName, volumeName string) (func(), error) {
				result, createErr := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
					Config: &containerTypes.Config{Image: "busybox:latest", Labels: migrationTestLabels(stackName)},
					HostConfig: &containerTypes.HostConfig{Mounts: []mount.Mount{{
						Type: mount.TypeVolume, Source: volumeName, Target: "/data",
					}}},
					Name: stackName + "-service-1",
				})
				if createErr != nil {
					return nil, createErr
				}

				return func() { //nolint:contextcheck // cleanup must run after the test context is cancelled.
					_, _ = apiClient.ContainerRemove(context.Background(), result.ID, client.ContainerRemoveOptions{Force: true})
				}, nil
			},
			assertGone: func(stackName string) error {
				containers, listErr := GetLabeledContainers(ctx, apiClient, api.ProjectLabel, stackName, true)
				if listErr != nil {
					return listErr
				}

				if len(containers) != 0 {
					return fmt.Errorf("expected Compose project to be removed, found %d containers", len(containers))
				}

				return nil
			},
		},
		{
			name:       "swarm to compose",
			sourceMode: true,
			create: func(stackName, volumeName string) (func(), error) {
				replicas := uint64(0)

				result, createErr := apiClient.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: swarmTypes.ServiceSpec{
					Annotations: swarmTypes.Annotations{Name: stackName + "_service", Labels: migrationTestLabels(stackName)},
					TaskTemplate: swarmTypes.TaskSpec{ContainerSpec: &swarmTypes.ContainerSpec{
						Image:  "busybox:latest",
						Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volumeName, Target: "/data"}},
					}},
					Mode: swarmTypes.ServiceMode{Replicated: &swarmTypes.ReplicatedService{Replicas: &replicas}},
				}})
				if createErr != nil {
					return nil, createErr
				}

				return func() { //nolint:contextcheck // cleanup must run after the test context is cancelled.
					_, _ = apiClient.ServiceRemove(context.Background(), result.ID, client.ServiceRemoveOptions{})
				}, nil
			},
			assertGone: func(stackName string) error {
				services, listErr := GetServiceLabels(ctx, apiClient, true, stackName)
				if listErr != nil {
					return listErr
				}

				if len(services) != 0 {
					return fmt.Errorf("expected Swarm stack to be removed, found %d services", len(services))
				}

				return nil
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stackName := test.ConvertTestName(t.Name())

			volumeName := stackName + "-data"
			if _, err := apiClient.VolumeCreate(ctx, client.VolumeCreateOptions{Name: volumeName}); err != nil {
				t.Fatalf("failed to create named volume: %v", err)
			}

			t.Cleanup(func() { //nolint:contextcheck // cleanup must run after the test context is cancelled.
				_, _ = apiClient.VolumeRemove(context.Background(), volumeName, client.VolumeRemoveOptions{Force: true})
			})

			cleanupResource, err := tt.create(stackName, volumeName)
			if err != nil {
				t.Fatalf("failed to create source deployment: %v", err)
			}

			t.Cleanup(cleanupResource)

			if err := MigrateDeploymentMode(ctx, nil, dockerCli, "", stackName, "owner/repo", !tt.sourceMode, true); err != nil {
				t.Fatalf("MigrateDeploymentMode() error = %v", err)
			}

			if err := tt.assertGone(stackName); err != nil {
				t.Fatal(err)
			}

			if _, err := apiClient.VolumeInspect(ctx, volumeName, client.VolumeInspectOptions{}); err != nil {
				t.Fatalf("expected named volume to survive migration: %v", err)
			}
		})
	}
}

func migrationTestLabels(stackName string) map[string]string {
	return map[string]string{
		api.ProjectLabel:              stackName,
		swarm.StackNamespaceLabel:     stackName,
		DocoCDLabels.Metadata.Manager: app.Name,
		DocoCDLabels.Source.Name:      "owner/repo",
	}
}
