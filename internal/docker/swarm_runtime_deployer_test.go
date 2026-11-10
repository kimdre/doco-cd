package docker

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"testing"

	"github.com/containerd/errdefs"
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

	const (
		oldImage = "busybox:1.34.1"
		newImage = "busybox:1.33.1"
	)

	oldRef := pullImageWithDigestRef(ctx, t, apiClient, oldImage)
	newRef := pullImageWithDigestRef(ctx, t, apiClient, newImage)

	t.Cleanup(func() {
		for _, image := range []string{oldImage, newImage} {
			_, _ = apiClient.ImageRemove(context.Background(), image, client.ImageRemoveOptions{})
		}
	})

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

	t.Cleanup(func() {
		_, _ = apiClient.ServiceRemove(context.Background(), created.ID, client.ServiceRemoveOptions{})
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

func TestReplacedImageRefs(t *testing.T) {
	t.Parallel()

	const (
		app136  = "busybox:1.36@sha256:aaa"
		app137  = "busybox:1.37@sha256:bbb"
		nginx   = "nginx:1.27@sha256:ccc"
		alpine  = "alpine:3.22@sha256:ddd"
		alpine3 = "alpine:3.23@sha256:eee"
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
			want:   []string{app136},
		},
		{
			name:   "service removed",
			before: map[string]string{"app": app136, "web": nginx},
			after:  map[string]string{"app": app136},
			want:   []string{nginx},
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
			want:   []string{app136},
		},
		{
			name:   "new tag for the same digest",
			before: map[string]string{"app": "busybox:1.36@sha256:aaa"},
			after:  map[string]string{"app": "busybox:stable@sha256:aaa"},
			want:   nil,
		},
		{
			name:   "still used by another stack",
			before: map[string]string{"app": app136},
			after:  map[string]string{"svc-a": app137, "svc-b": app136},
			want:   nil,
		},
		{
			name:   "references without digest",
			before: map[string]string{"app": "app:1", "web": "web:latest"},
			after:  map[string]string{"app": "app:2", "web": "web:latest"},
			want:   []string{"app:1"},
		},
		{
			name:   "several replaced and shared",
			before: map[string]string{"app": app136, "worker": app136, "sidecar": alpine},
			after:  map[string]string{"app": app137, "worker": app137, "sidecar": alpine3},
			want:   []string{alpine, app136},
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
