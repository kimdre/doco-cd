package docker

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	containerTypes "github.com/moby/moby/api/types/container"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/test"
)

func TestSwarmOneOffServiceName(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 1730000000000000000)
	suffix := "-doco-job-1730000000000000000"

	testCases := []struct {
		name              string
		sourceServiceName string
		want              string
	}{
		{
			name:              "keeps short source service name",
			sourceServiceName: "stack_backup",
			want:              "stack_backup" + suffix,
		},
		{
			name:              "truncates source service name to Docker limit",
			sourceServiceName: strings.Repeat("a", maxSwarmServiceNameLength),
			want:              strings.Repeat("a", maxSwarmServiceNameLength-len(suffix)) + suffix,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := swarmOneOffServiceName(tc.sourceServiceName, now)

			if got != tc.want {
				t.Errorf("swarmOneOffServiceName() = %q, want %q", got, tc.want)
			}

			if len(got) > maxSwarmServiceNameLength {
				t.Errorf("swarmOneOffServiceName() length = %d, want <= %d", len(got), maxSwarmServiceNameLength)
			}
		})
	}
}

func TestRunSwarmJob(t *testing.T) {
	t.Parallel()

	dockerCli, err := CreateDockerCli(false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	if !resolveTestSwarmMode(t.Context(), t, dockerCli.Client()) {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	testCases := []struct {
		mode    swarm.DeployMode
		command []string
		title   string
		wantErr string
	}{
		{mode: swarm.DeployModeGlobalJob, command: []string{"docker", "info"}, title: "global-docker-info"},
		{mode: swarm.DeployModeReplicatedJob, command: []string{"docker", "info"}, title: "replicated-docker-info"},
		{mode: swarm.DeployModeReplicatedJob, command: []string{"sh", "-c", "exit 7"}, title: "replicated-exit-7", wantErr: "exit code 7"},
	}

	for _, tc := range testCases {
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Parallel()

			t.Logf("Running job with mode: %s, command: %v, title: %s", tc.mode, tc.command, tc.title)

			err := RunSwarmJob(t.Context(), dockerCli, tc.mode, tc.command, tc.title)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("RunSwarmJob failed: %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("RunSwarmJob error = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestRunImageRemoveJob(t *testing.T) {
	t.Parallel()

	dockerCli, err := CreateDockerCli(false)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	apiClient := dockerCli.Client()
	ctx := t.Context()

	if !resolveTestSwarmMode(ctx, t, apiClient) {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	repo := "doco-cd-test/" + test.ConvertTestName(t.Name())
	replacedRef := repo + ":replaced"
	runningRef := repo + ":running"

	commitUniqueTestImage(ctx, t, apiClient, replacedRef)
	commitUniqueTestImage(ctx, t, apiClient, runningRef)

	// A stopped container stands in for a task Swarm keeps in its task history.
	createImageRemoveTestContainer(ctx, t, apiClient, replacedRef, false)
	createImageRemoveTestContainer(ctx, t, apiClient, runningRef, true)

	missingRef := repo + "@sha256:" + strings.Repeat("0", 64)
	retry := imageRemoveRetry{attempts: 2, period: time.Second}
	name := imageRemoveJobPrefix + test.ConvertTestName(t.Name())

	if err = runImageRemoveJob(ctx, dockerCli, name, []string{replacedRef, runningRef, missingRef}, retry); err != nil {
		t.Fatalf("runImageRemoveJob failed: %v", err)
	}

	if _, err = apiClient.ImageInspect(ctx, replacedRef); !errdefs.IsNotFound(err) {
		t.Errorf("expected %s to be removed, inspect error: %v", replacedRef, err)
	}

	if _, err = apiClient.ImageInspect(ctx, runningRef); err != nil {
		t.Errorf("expected %s used by a running container to be kept: %v", runningRef, err)
	}

	assertSwarmServiceRemoved(ctx, t, apiClient, name)

	badName := name + "-invalid"

	err = runImageRemoveJob(ctx, dockerCli, badName, []string{"Invalid::Ref"}, retry)
	if err == nil {
		t.Error("expected an invalid reference to fail the job")
	}

	assertSwarmServiceRemoved(ctx, t, apiClient, badName)
}

func assertSwarmServiceRemoved(ctx context.Context, t *testing.T, apiClient client.APIClient, name string) {
	t.Helper()

	if _, err := apiClient.ServiceInspect(ctx, name, client.ServiceInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Errorf("expected job service %s to be removed after the run, inspect error: %v", name, err)
	}
}

// commitUniqueTestImage commits busybox:latest as a new image tagged ref, so removing it by ID
// cannot touch an image another test or workload on the daemon uses.
func commitUniqueTestImage(ctx context.Context, t *testing.T, apiClient client.APIClient, ref string) {
	t.Helper()

	const base = "busybox:latest"

	if _, err := apiClient.ImageInspect(ctx, base); err != nil {
		reader, pullErr := apiClient.ImagePull(ctx, base, client.ImagePullOptions{})
		if pullErr != nil {
			t.Skipf("cannot pull %s: %v", base, pullErr)
		}

		_, _ = io.Copy(io.Discard, reader)
		_ = reader.Close()
	}

	created, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &containerTypes.Config{Image: base}})
	if err != nil {
		t.Fatalf("failed to create container from %s: %v", base, err)
	}

	defer func() {
		_, _ = apiClient.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	}()

	committed, err := apiClient.ContainerCommit(ctx, created.ID, client.ContainerCommitOptions{
		Reference: ref,
		Changes:   []string{"LABEL cd.doco.test.image=" + ref},
	})
	if err != nil {
		t.Fatalf("failed to commit %s: %v", ref, err)
	}

	// Registered before the containers that use it, so it runs after their cleanups.
	t.Cleanup(func() {
		_, _ = apiClient.ImageRemove(context.WithoutCancel(ctx), committed.ID, client.ImageRemoveOptions{Force: true})
	})
}

func createImageRemoveTestContainer(ctx context.Context, t *testing.T, apiClient client.APIClient, image string, start bool) {
	t.Helper()

	result, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &containerTypes.Config{
			Image: image,
			Cmd:   []string{"sleep", "300"},
		},
		Name: test.ConvertTestName(t.Name()) + "-" + strings.NewReplacer("/", "-", ":", "-", ".", "-").Replace(image),
	})
	if err != nil {
		t.Fatalf("failed to create container from %s: %v", image, err)
	}

	t.Cleanup(func() { //nolint:contextcheck // cleanup must run after the test context is cancelled.
		_, _ = apiClient.ContainerRemove(context.Background(), result.ID, client.ContainerRemoveOptions{Force: true})
	})

	if !start {
		return
	}

	if _, err = apiClient.ContainerStart(ctx, result.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("failed to start container from %s: %v", image, err)
	}
}

type staleJobsTestClient struct {
	client.APIClient
	services []swarmTypes.Service
	removed  []string
}

func (c *staleJobsTestClient) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	return client.ServiceListResult{Items: c.services}, nil
}

func (c *staleJobsTestClient) ServiceRemove(_ context.Context, id string, _ client.ServiceRemoveOptions) (client.ServiceRemoveResult, error) {
	c.removed = append(c.removed, id)
	return client.ServiceRemoveResult{}, nil
}

func TestRemoveStaleImageRemoveJobs(t *testing.T) {
	t.Parallel()

	now := time.Now()
	old := now.Add(-2 * staleImageRemoveJobAge)
	recent := now.Add(-time.Minute)

	service := func(id, name, trigger string, updatedAt time.Time) swarmTypes.Service {
		svc := swarmTypes.Service{ID: id}
		svc.UpdatedAt = updatedAt
		svc.Spec.Name = name
		svc.Spec.Labels = map[string]string{DocoCDLabels.Deployment.Trigger: trigger}

		return svc
	}

	apiClient := &staleJobsTestClient{services: []swarmTypes.Service{
		service("leftover", imageRemoveJobPrefix+"1", imageRemoveJobTitle, old),
		service("running", imageRemoveJobPrefix+"2", imageRemoveJobTitle, recent),
		service("legacy", legacyImagePruneJobName, "image-prune", old),
		service("legacy-in-use", legacyImagePruneJobName, "image-prune", recent),
		service("other-trigger", imageRemoveJobPrefix+"3", "webhook", old),
		service("stack-service", "app_image-remove-worker", imageRemoveJobTitle, old),
	}}

	removeStaleImageRemoveJobs(t.Context(), apiClient, now, nil)

	if want := []string{"leftover", "legacy"}; !slices.Equal(apiClient.removed, want) {
		t.Errorf("removed services = %v, want %v", apiClient.removed, want)
	}
}
