package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	containerTypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/test"
)

type serviceImagesTestClient struct {
	client.APIClient
	services []swarmTypes.Service
	err      error
}

func (c *serviceImagesTestClient) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	return client.ServiceListResult{Items: c.services}, c.err
}

func TestClusterServiceImageRefs(t *testing.T) {
	t.Parallel()

	withImage := func(id, image string) swarmTypes.Service {
		svc := swarmTypes.Service{ID: id}
		svc.Spec.TaskTemplate.ContainerSpec = &swarmTypes.ContainerSpec{Image: image}

		return svc
	}

	apiClient := &serviceImagesTestClient{services: []swarmTypes.Service{
		withImage("a", "busybox:1.37@sha256:bbb"),
		{ID: "no-container-spec"},
		withImage("b", "nginx:1.27@sha256:ccc"),
	}}

	got, err := clusterServiceImageRefs(t.Context(), apiClient)
	if err != nil {
		t.Fatalf("clusterServiceImageRefs() error = %v", err)
	}

	want := map[string]string{"a": "busybox:1.37@sha256:bbb", "b": "nginx:1.27@sha256:ccc"}
	if !maps.Equal(got, want) {
		t.Errorf("clusterServiceImageRefs() = %v, want %v", got, want)
	}

	apiClient.err = errors.New("daemon unavailable")
	if _, err = clusterServiceImageRefs(t.Context(), apiClient); !errors.Is(err, apiClient.err) {
		t.Errorf("clusterServiceImageRefs() error = %v, want it to wrap %v", err, apiClient.err)
	}
}

// TestRemoveReplacedSwarmImages checks that a replaced image is removed even though the old task's stopped container still references it.
func TestRemoveReplacedSwarmImages(t *testing.T) {
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

	stackName := test.ConvertTestName(t.Name())
	logger := slog.New(slog.DiscardHandler)

	repo := startTestRegistry(ctx, t, apiClient) + "/" + stackName
	oldRef := pushUniqueTestImage(ctx, t, apiClient, repo+":old")
	newRef := pushUniqueTestImage(ctx, t, apiClient, repo+":new")

	replicas := uint64(1)

	created, err := apiClient.ServiceCreate(ctx, client.ServiceCreateOptions{
		Spec: swarmTypes.ServiceSpec{
			Name:   stackName + "_app",
			Labels: map[string]string{swarm.StackNamespaceLabel: stackName},
			TaskTemplate: swarmTypes.TaskSpec{
				ContainerSpec: &swarmTypes.ContainerSpec{Image: oldRef, Command: []string{"sleep", "300"}},
			},
			Mode: swarmTypes.ServiceMode{Replicated: &swarmTypes.ReplicatedService{Replicas: &replicas}},
		},
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	// The task container outlives the service briefly, and the image cleanups that run after
	// this one cannot remove an image a running container still uses.
	t.Cleanup(func() {
		_, _ = apiClient.ServiceRemove(context.Background(), created.ID, client.ServiceRemoveOptions{})

		filter := make(client.Filters).Add("label", "com.docker.swarm.service.id="+created.ID)
		for range 60 {
			result, err := apiClient.ContainerList(context.Background(), client.ContainerListOptions{All: true, Filters: filter})
			if err != nil || len(result.Items) == 0 {
				return
			}

			time.Sleep(500 * time.Millisecond)
		}
	})

	if err = swarm.WaitOnServices(ctx, dockerCli, []string{created.ID}); err != nil {
		t.Fatalf("service did not converge: %v", err)
	}

	before, err := getDeployedServiceImageRefs(ctx, dockerCli, true, stackName, logger)
	if err != nil {
		t.Fatalf("failed to get images before update: %v", err)
	}

	inspect, err := apiClient.ServiceInspect(ctx, created.ID, client.ServiceInspectOptions{})
	if err != nil {
		t.Fatalf("failed to inspect service: %v", err)
	}

	spec := inspect.Service.Spec
	spec.TaskTemplate.ContainerSpec.Image = newRef

	if _, err = apiClient.ServiceUpdate(ctx, created.ID, client.ServiceUpdateOptions{Version: inspect.Service.Version, Spec: spec}); err != nil {
		t.Fatalf("failed to update service: %v", err)
	}

	if err = swarm.WaitOnServices(ctx, dockerCli, []string{created.ID}); err != nil {
		t.Fatalf("service did not converge after update: %v", err)
	}

	req := runtimeDeployRequest{
		request:  DeployRequest{DockerCLI: dockerCli, DeployConfig: &deploy.Config{Name: stackName}},
		phase:    newDeploymentPhaseState("", nil),
		stackLog: logger,
	}

	// Nothing ran before the first deployment, so no job is started.
	if err = removeReplacedSwarmImages(ctx, req, nil); err != nil {
		t.Fatalf("removeReplacedSwarmImages without previous images failed: %v", err)
	}

	if _, err = apiClient.ImageInspect(ctx, oldRef); err != nil {
		t.Fatalf("expected %s to be kept without previous images: %v", oldRef, err)
	}

	if err = removeReplacedSwarmImages(ctx, req, before); err != nil {
		t.Fatalf("removeReplacedSwarmImages failed: %v", err)
	}

	if _, err = apiClient.ImageInspect(ctx, oldRef); !errdefs.IsNotFound(err) {
		t.Errorf("expected replaced image %s to be removed, inspect error: %v", oldRef, err)
	}

	if _, err = apiClient.ImageInspect(ctx, newRef); err != nil {
		t.Errorf("expected image %s of the running task to be kept: %v", newRef, err)
	}
}

// startTestRegistry runs a registry on a loopback port, which the daemon trusts over plain HTTP,
// and returns its address. Pushing to it gives a test image the repo digest Swarm pins services to.
func startTestRegistry(ctx context.Context, t *testing.T, apiClient client.APIClient) string {
	t.Helper()

	const image = "registry:2"

	reader, err := apiClient.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		t.Skipf("cannot pull %s: %v", image, err)
	}

	_, _ = io.Copy(io.Discard, reader)
	_ = reader.Close()

	port := network.MustParsePort("5000/tcp")

	created, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &containerTypes.Config{Image: image, ExposedPorts: network.PortSet{port: {}}},
		HostConfig: &containerTypes.HostConfig{
			PortBindings: network.PortMap{port: {{HostIP: netip.MustParseAddr("127.0.0.1")}}},
		},
		Name: test.ConvertTestName(t.Name()) + "-registry",
	})
	if err != nil {
		t.Fatalf("failed to create registry container: %v", err)
	}

	t.Cleanup(func() { //nolint:contextcheck // cleanup must run after the test context is cancelled.
		_, _ = apiClient.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	})

	if _, err = apiClient.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("failed to start registry container: %v", err)
	}

	inspect, err := apiClient.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("failed to inspect registry container: %v", err)
	}

	bindings := inspect.Container.NetworkSettings.Ports[port]
	if len(bindings) == 0 {
		t.Fatal("registry container has no published port")
	}

	return "127.0.0.1:" + bindings[0].HostPort
}

// pushUniqueTestImage commits a unique image as ref, pushes it and returns it in the
// tag@digest form Swarm stores in service specs.
func pushUniqueTestImage(ctx context.Context, t *testing.T, apiClient client.APIClient, ref string) string {
	t.Helper()

	commitUniqueTestImage(ctx, t, apiClient, ref)

	var pushErr error

	// The registry may still be starting, so retry the push briefly.
	for range 20 {
		var push client.ImagePushResponse

		push, pushErr = apiClient.ImagePush(ctx, ref, client.ImagePushOptions{RegistryAuth: "e30="})
		if pushErr == nil {
			pushErr = push.Wait(ctx)
			_ = push.Close()
		}

		if pushErr == nil {
			break
		}

		time.Sleep(500 * time.Millisecond)
	}

	if pushErr != nil {
		t.Fatalf("failed to push %s: %v", ref, pushErr)
	}

	inspect, err := apiClient.ImageInspect(ctx, ref)
	if err != nil {
		t.Fatalf("failed to inspect %s: %v", ref, err)
	}

	digest := digestFromRepoDigests(inspect.RepoDigests)
	if digest == "" {
		t.Fatalf("%s has no repo digest after the push", ref)
	}

	return ref + "@" + digest
}

func TestReplacedImageRefs(t *testing.T) {
	t.Parallel()

	digest := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }

	var (
		app136  = "busybox:1.36@" + digest("a")
		app137  = "busybox:1.37@" + digest("b")
		nginx   = "nginx:1.27@" + digest("c")
		alpine  = "alpine:3.22@" + digest("d")
		alpine3 = "alpine:3.23@" + digest("e")
	)

	tests := []struct {
		name   string
		before map[string]string
		after  map[string]string
		want   []string
	}{
		{
			name:   "first deployment",
			before: nil,
			after:  map[string]string{"app": app136},
			want:   nil,
		},
		{
			name:   "unchanged",
			before: map[string]string{"app": app136, "web": nginx},
			after:  map[string]string{"app": app136, "web": nginx},
			want:   nil,
		},
		{
			name:   "tag changed",
			before: map[string]string{"app": app136, "web": nginx},
			after:  map[string]string{"app": app137, "web": nginx},
			want:   []string{"busybox@" + digest("a")},
		},
		{
			name:   "service removed",
			before: map[string]string{"app": app136, "web": nginx},
			after:  map[string]string{"app": app136},
			want:   []string{"nginx@" + digest("c")},
		},
		{
			name:   "still used by another service",
			before: map[string]string{"app": app136, "worker": app136},
			after:  map[string]string{"app": app137, "worker": app136},
			want:   nil,
		},
		{
			name:   "image moved to another service",
			before: map[string]string{"app": app136, "web": nginx},
			after:  map[string]string{"app": nginx, "web": app137},
			want:   []string{"busybox@" + digest("a")},
		},
		{
			name:   "new tag for the same digest",
			before: map[string]string{"app": "busybox:1.36@" + digest("a")},
			after:  map[string]string{"app": "busybox:stable@" + digest("a")},
			want:   nil,
		},
		{
			name:   "digest-only reference to the same image",
			before: map[string]string{"app": app136},
			after:  map[string]string{"app": app137, "job": "docker.io/library/busybox@" + digest("a")},
			want:   nil,
		},
		{
			name:   "still used by another stack",
			before: map[string]string{"app": app136},
			after:  map[string]string{"svc-a": app137, "svc-b": app136},
			want:   nil,
		},
		{
			name:   "references without digest are never removed",
			before: map[string]string{"app": "app:1", "web": "web:latest"},
			after:  map[string]string{"app": "app:2", "web": "web:latest"},
			want:   nil,
		},
		{
			name:   "reference without digest whose repository is gone",
			before: map[string]string{"app": "app:1"},
			after:  map[string]string{"app": app137},
			want:   nil,
		},
		{
			name:   "repository still referenced without digest",
			before: map[string]string{"app": app136, "web": nginx},
			after:  map[string]string{"app": app137, "web": nginx, "scheduled": "busybox:1.36"},
			want:   nil,
		},
		{
			name:   "unparsable reference in use",
			before: map[string]string{"app": app136},
			after:  map[string]string{"app": app137, "other": "Invalid::Ref"},
			want:   nil,
		},
		{
			name:   "unparsable previous reference",
			before: map[string]string{"app": app136, "web": "Invalid::Ref"},
			after:  map[string]string{"app": app137},
			want:   []string{"busybox@" + digest("a")},
		},
		{
			name:   "several replaced and shared",
			before: map[string]string{"app": app136, "worker": app136, "sidecar": alpine},
			after:  map[string]string{"app": app137, "worker": app137, "sidecar": alpine3},
			want:   []string{"alpine@" + digest("d"), "busybox@" + digest("a")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := replacedImageRefs(tc.before, tc.after)
			if !slices.Equal(got, tc.want) {
				t.Errorf("replacedImageRefs() = %v, want %v", got, tc.want)
			}
		})
	}
}
