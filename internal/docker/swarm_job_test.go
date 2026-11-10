package docker

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	containerTypes "github.com/moby/moby/api/types/container"
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

	// Pinned tags no other test pulls, so removing them cannot race another test.
	const (
		replacedImage = "busybox:1.36.1"
		runningImage  = "busybox:1.35.0"
	)

	replacedRef := pullImageWithDigestRef(ctx, t, apiClient, replacedImage)
	runningRef := pullImageWithDigestRef(ctx, t, apiClient, runningImage)

	// Registered first so it runs after the container cleanups.
	t.Cleanup(func() {
		for _, image := range []string{replacedImage, runningImage} {
			_, _ = apiClient.ImageRemove(context.Background(), image, client.ImageRemoveOptions{})
		}
	})

	// A stopped container stands in for a task Swarm keeps in its task history.
	createImageRemoveTestContainer(ctx, t, apiClient, replacedImage, false)
	createImageRemoveTestContainer(ctx, t, apiClient, runningImage, true)

	missingRef := "busybox@sha256:0000000000000000000000000000000000000000000000000000000000000000"

	if err = RunImageRemoveJob(ctx, dockerCli, []string{replacedRef, runningRef, missingRef}); err != nil {
		t.Fatalf("RunImageRemoveJob failed: %v", err)
	}

	if _, err = apiClient.ImageInspect(ctx, replacedRef); !errdefs.IsNotFound(err) {
		t.Errorf("expected %s to be removed, inspect error: %v", replacedRef, err)
	}

	for _, ref := range []string{runningRef, runningImage} {
		if _, err = apiClient.ImageInspect(ctx, ref); err != nil {
			t.Errorf("expected %s used by a running container to be kept: %v", ref, err)
		}
	}
}

// pullImageWithDigestRef pulls image and returns it in the tag@digest form Swarm stores in service specs.
func pullImageWithDigestRef(ctx context.Context, t *testing.T, apiClient client.APIClient, image string) string {
	t.Helper()

	reader, err := apiClient.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		t.Skipf("cannot pull %s: %v", image, err)
	}

	_, _ = io.Copy(io.Discard, reader)
	_ = reader.Close()

	inspect, err := apiClient.ImageInspect(ctx, image)
	if err != nil {
		t.Fatalf("failed to inspect %s: %v", image, err)
	}

	digest := digestFromRepoDigests(inspect.RepoDigests)
	if digest == "" {
		t.Fatalf("%s has no repo digest", image)
	}

	return image + "@" + digest
}

func createImageRemoveTestContainer(ctx context.Context, t *testing.T, apiClient client.APIClient, image string, start bool) {
	t.Helper()

	result, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &containerTypes.Config{
			Image: image,
			Cmd:   []string{"sleep", "300"},
		},
		Name: test.ConvertTestName(t.Name()) + "-" + strings.NewReplacer(":", "-", ".", "-").Replace(image),
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
