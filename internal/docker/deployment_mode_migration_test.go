package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
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
	containers         []containerTypes.Summary
	services           []swarmTypes.Service
	containerListCalls int
	serviceListCalls   int
}

func (c *deploymentModeMigrationClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	c.containerListCalls++

	return client.ContainerListResult{Items: c.containers}, nil
}

func (c *deploymentModeMigrationClient) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	c.serviceListCalls++

	return client.ServiceListResult{Items: c.services}, nil
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

	dockerCli := deploymentModeMigrationCLI{apiClient: &deploymentModeMigrationClient{
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

func TestMigrateDeploymentMode_RejectsUnmanagedSelectedMode(t *testing.T) {
	t.Parallel()

	apiClient := &deploymentModeMigrationClient{
		containers: []containerTypes.Summary{{
			Names:  []string{"/example_web_1"},
			Labels: migrationTestOwnershipLabels(),
		}},
		services: []swarmTypes.Service{{
			Spec: swarmTypes.ServiceSpec{
				Annotations: swarmTypes.Annotations{
					Name:   "example_web",
					Labels: map[string]string{swarm.StackNamespaceLabel: "example"},
				},
			},
		}},
	}
	dockerCli := deploymentModeMigrationCLI{apiClient: apiClient}

	err := MigrateDeploymentMode(t.Context(), nil, dockerCli, "", "example", "owner/repo", true, true)
	if !errors.Is(err, ErrDeploymentModeConflict) {
		t.Fatalf("MigrateDeploymentMode() error = %v, want ErrDeploymentModeConflict", err)
	}

	if apiClient.containerListCalls != 1 || apiClient.serviceListCalls != 1 {
		t.Fatalf("unexpected migration API calls: ContainerList=%d ServiceList=%d", apiClient.containerListCalls, apiClient.serviceListCalls)
	}
}

func TestMigrateDeploymentMode_SkipsMigrationWhenSwarmUnavailable(t *testing.T) {
	t.Parallel()

	apiClient := &deploymentModeMigrationClient{}
	dockerCli := deploymentModeMigrationCLI{apiClient: apiClient}

	if err := MigrateDeploymentMode(t.Context(), nil, dockerCli, "", "example", "owner/repo", false, false); err != nil {
		t.Fatalf("MigrateDeploymentMode() error = %v, want nil", err)
	}

	if apiClient.containerListCalls != 0 || apiClient.serviceListCalls != 0 {
		t.Fatalf("expected no Docker API calls, got ContainerList=%d ServiceList=%d", apiClient.containerListCalls, apiClient.serviceListCalls)
	}
}

func TestMigrationSourceMatches(t *testing.T) {
	t.Parallel()

	expectedSources := buildRepositoryLabelCandidates("owner/repo")

	for _, tt := range []struct {
		name   string
		labels Labels
		want   bool
	}{
		{
			name:   "source name",
			labels: Labels{DocoCDLabels.Source.Name: "owner/repo"},
			want:   true,
		},
		{
			name:   "source URL",
			labels: Labels{DocoCDLabels.Source.URL: "https://github.com/owner/repo.git"},
			want:   true,
		},
		{
			name:   "different repository",
			labels: Labels{DocoCDLabels.Source.Name: "owner/other"},
			want:   false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := migrationSourceMatches(tt.labels, expectedSources); got != tt.want {
				t.Fatalf("migrationSourceMatches() = %t, want %t", got, tt.want)
			}
		})
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
		create     func(*testing.T, string, string) error
		assertGone func(string) error
	}{
		{
			name:       "compose to swarm",
			sourceMode: false,
			create: func(t *testing.T, stackName, volumeName string) error {
				test.ComposeUp(ctx, t,
					test.WithName(stackName),
					test.WithYAML(migrationVolumeServiceYAML(volumeName)),
					test.WithCustomLabel(migrationTestOwnershipLabels()),
				)

				return nil
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
			create: func(t *testing.T, stackName, volumeName string) error {
				replicas := uint64(1)

				result, createErr := apiClient.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: swarmTypes.ServiceSpec{
					Annotations: swarmTypes.Annotations{Name: stackName + "_service", Labels: swarmMigrationTestLabels(stackName)},
					TaskTemplate: swarmTypes.TaskSpec{ContainerSpec: &swarmTypes.ContainerSpec{
						Image:   "busybox:latest",
						Command: []string{"sh", "-c", "while true; do sleep 3600; done"},
						Mounts:  []mount.Mount{{Type: mount.TypeVolume, Source: volumeName, Target: "/data"}},
					}},
					Mode: swarmTypes.ServiceMode{Replicated: &swarmTypes.ReplicatedService{Replicas: &replicas}},
				}})
				if createErr != nil {
					return createErr
				}

				t.Cleanup(func() { //nolint:contextcheck // cleanup must run after the test context is cancelled.
					_, _ = apiClient.ServiceRemove(context.Background(), result.ID, client.ServiceRemoveOptions{})
				})

				return nil
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

			if err := tt.create(t, stackName, volumeName); err != nil {
				t.Fatalf("failed to create source deployment: %v", err)
			}

			sourceContainerID := migrationServiceContainerID(ctx, t, apiClient, stackName, tt.sourceMode)
			writeMigrationVolumeMarker(ctx, t, apiClient, sourceContainerID)

			if err := MigrateDeploymentMode(ctx, nil, dockerCli, "", stackName, "owner/repo", !tt.sourceMode, true); err != nil {
				t.Fatalf("MigrateDeploymentMode() error = %v", err)
			}

			if err := tt.assertGone(stackName); err != nil {
				t.Fatal(err)
			}

			if _, err := apiClient.VolumeInspect(ctx, volumeName, client.VolumeInspectOptions{}); err != nil {
				t.Fatalf("expected named volume to survive migration: %v", err)
			}

			var targetContainerID string

			if tt.sourceMode {
				targetStack := test.ComposeUp(ctx, t,
					test.WithName(stackName),
					test.WithYAML(migrationVolumeServiceYAML(volumeName)),
					test.WithCustomLabel(migrationTestOwnershipLabels()),
				)
				targetContainerID = targetStack.ServiceContainerID(ctx, t, "service")
			} else {
				createMigrationSwarmVolumeService(ctx, t, apiClient, stackName, volumeName)
				targetContainerID = migrationServiceContainerID(ctx, t, apiClient, stackName, true)
			}

			readMigrationVolumeMarker(ctx, t, apiClient, targetContainerID)
		})
	}
}

func migrationVolumeServiceYAML(volumeName string) string {
	return fmt.Sprintf(`services:
  service:
    image: busybox:latest
    command: ["sh", "-c", "while true; do sleep 3600; done"]
    volumes:
      - data:/data
volumes:
  data:
    external: true
    name: %s
`, volumeName)
}

func createMigrationSwarmVolumeService(ctx context.Context, t *testing.T, apiClient client.APIClient, stackName, volumeName string) {
	t.Helper()

	replicas := uint64(1)

	result, err := apiClient.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: swarmTypes.ServiceSpec{
		Annotations: swarmTypes.Annotations{Name: stackName + "_service", Labels: swarmMigrationTestLabels(stackName)},
		TaskTemplate: swarmTypes.TaskSpec{ContainerSpec: &swarmTypes.ContainerSpec{
			Image:   "busybox:latest",
			Command: []string{"sh", "-c", "while true; do sleep 3600; done"},
			Mounts:  []mount.Mount{{Type: mount.TypeVolume, Source: volumeName, Target: "/data"}},
		}},
		Mode: swarmTypes.ServiceMode{Replicated: &swarmTypes.ReplicatedService{Replicas: &replicas}},
	}})
	if err != nil {
		t.Fatalf("failed to create target Swarm deployment: %v", err)
	}

	t.Cleanup(func() { //nolint:contextcheck // cleanup must run after the test context is cancelled.
		_, _ = apiClient.ServiceRemove(context.Background(), result.ID, client.ServiceRemoveOptions{})
	})
}

func migrationServiceContainerID(ctx context.Context, t *testing.T, apiClient client.APIClient, stackName string, swarmMode bool) string {
	t.Helper()

	var containerID string

	err := retry.New(
		retry.Attempts(21),
		retry.Delay(250*time.Millisecond),
		retry.DelayType(retry.FixedDelay),
	).Do(func() error {
		if swarmMode {
			tasks, err := apiClient.TaskList(ctx, client.TaskListOptions{
				Filters: make(client.Filters).Add("label", swarm.StackNamespaceLabel+"="+stackName),
			})
			if err != nil {
				return err
			}

			for _, task := range tasks.Items {
				if task.Status.ContainerStatus != nil && task.Status.ContainerStatus.ContainerID != "" {
					containerID = task.Status.ContainerStatus.ContainerID

					return nil
				}
			}

			return fmt.Errorf("no running Swarm task container for stack %q", stackName)
		}

		containers, err := GetLabeledContainers(ctx, apiClient, api.ProjectLabel, stackName, false)
		if err != nil {
			return err
		}

		if len(containers) != 1 {
			return fmt.Errorf("expected one Compose container for stack %q, got %d", stackName, len(containers))
		}

		containerID = containers[0].ID

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return containerID
}

func writeMigrationVolumeMarker(ctx context.Context, t *testing.T, apiClient client.APIClient, containerID string) {
	t.Helper()

	if _, err := ExecContext(ctx, apiClient, containerID, "sh", "-c", "printf migration-data >/data/migration-marker"); err != nil {
		t.Fatalf("failed to write named-volume marker: %v", err)
	}
}

func readMigrationVolumeMarker(ctx context.Context, t *testing.T, apiClient client.APIClient, containerID string) {
	t.Helper()

	output, err := ExecContext(ctx, apiClient, containerID, "cat", "/data/migration-marker")
	if err != nil {
		t.Fatalf("failed to read named-volume marker: %v", err)
	}

	if strings.TrimSpace(output) != "migration-data" {
		t.Fatalf("named-volume marker = %q, want %q", output, "migration-data")
	}
}

func migrationTestOwnershipLabels() map[string]string {
	return map[string]string{
		DocoCDLabels.Metadata.Manager: app.Name,
		DocoCDLabels.Source.Name:      "owner/repo",
	}
}

func swarmMigrationTestLabels(stackName string) map[string]string {
	labels := migrationTestOwnershipLabels()
	labels[swarm.StackNamespaceLabel] = stackName

	return labels
}
