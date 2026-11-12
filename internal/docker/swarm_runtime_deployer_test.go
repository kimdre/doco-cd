package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"strconv"
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

// TestRemoveReplacedSwarmImagesDefersStoppingTasks runs alone, not in parallel, because the
// deferred records it creates are cluster-wide and another run would pick them up.
func TestRemoveReplacedSwarmImagesDefersStoppingTasks(t *testing.T) {
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
	// sleep runs as PID 1 and ignores SIGTERM, so the old task keeps running for the whole grace period.
	gracePeriod := 5 * time.Minute

	created, err := apiClient.ServiceCreate(ctx, client.ServiceCreateOptions{
		Spec: swarmTypes.ServiceSpec{
			Name:   stackName + "_app",
			Labels: map[string]string{swarm.StackNamespaceLabel: stackName},
			TaskTemplate: swarmTypes.TaskSpec{
				ContainerSpec: &swarmTypes.ContainerSpec{Image: oldRef, Command: []string{"sleep", "600"}, StopGracePeriod: &gracePeriod},
			},
			Mode:         swarmTypes.ServiceMode{Replicated: &swarmTypes.ReplicatedService{Replicas: &replicas}},
			UpdateConfig: &swarmTypes.UpdateConfig{Order: swarmTypes.UpdateOrderStartFirst},
		},
	})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	serviceContainers := make(client.Filters).Add("label", "com.docker.swarm.service.id="+created.ID)

	removeContainers := func(image string) {
		result, listErr := apiClient.ContainerList(context.Background(), client.ContainerListOptions{All: true, Filters: serviceContainers})
		if listErr != nil {
			t.Errorf("failed to list task containers: %v", listErr)
			return
		}

		for _, c := range result.Items {
			if image == "" || c.Image == image {
				_, _ = apiClient.ContainerRemove(context.Background(), c.ID, client.ContainerRemoveOptions{Force: true})
			}
		}
	}

	t.Cleanup(func() {
		_, _ = apiClient.ServiceRemove(context.Background(), created.ID, client.ServiceRemoveOptions{})

		removeContainers("")

		deferred, _ := deferredImageRemovals(context.Background(), apiClient)
		for _, configID := range deferred {
			_, _ = apiClient.ConfigRemove(context.Background(), configID, client.ConfigRemoveOptions{})
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

	if err = removeReplacedSwarmImagesWithin(ctx, req, before, 2*time.Second); err != nil {
		t.Fatalf("removeReplacedSwarmImagesWithin failed: %v", err)
	}

	if _, err = apiClient.ImageInspect(ctx, oldRef); err != nil {
		t.Fatalf("expected %s of the stopping task to be kept: %v", oldRef, err)
	}

	oldRepo, oldDigest, _ := parseImageRef(oldRef)
	deferredRef := oldRepo + "@" + oldDigest

	deferred, err := deferredImageRemovals(ctx, apiClient)
	if err != nil {
		t.Fatalf("failed to list deferred image removals: %v", err)
	}

	if _, ok := deferred[deferredRef]; !ok {
		t.Fatalf("expected %s to be deferred, deferred images: %v", deferredRef, slices.Collect(maps.Keys(deferred)))
	}

	// Once the old task stops, a later run with no images of its own retries the deferred one.
	removeContainers(oldRef)

	if err = removeReplacedSwarmImagesWithin(ctx, req, nil, 30*time.Second); err != nil {
		t.Fatalf("retry of the deferred image failed: %v", err)
	}

	if _, err = apiClient.ImageInspect(ctx, oldRef); !errdefs.IsNotFound(err) {
		t.Errorf("expected deferred image %s to be removed on retry, inspect error: %v", oldRef, err)
	}

	if _, err = apiClient.ImageInspect(ctx, newRef); err != nil {
		t.Errorf("expected image %s of the running task to be kept: %v", newRef, err)
	}

	deferred, err = deferredImageRemovals(ctx, apiClient)
	if err != nil {
		t.Fatalf("failed to list deferred image removals: %v", err)
	}

	if _, ok := deferred[deferredRef]; ok {
		t.Errorf("expected the record of %s to be removed after the retry", deferredRef)
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

type imageTasksTestClient struct {
	client.APIClient
	tasks []swarmTypes.Task
	err   error
}

func (c *imageTasksTestClient) TaskList(context.Context, client.TaskListOptions) (client.TaskListResult, error) {
	return client.TaskListResult{Items: c.tasks}, c.err
}

func TestWaitForImageTasksStopped(t *testing.T) {
	t.Parallel()

	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	refA := "example.com/app@" + digestA

	task := func(image string, state swarmTypes.TaskState) swarmTypes.Task {
		return swarmTypes.Task{
			Spec:   swarmTypes.TaskSpec{ContainerSpec: &swarmTypes.ContainerSpec{Image: image}},
			Status: swarmTypes.TaskStatus{State: state},
		}
	}

	testCases := []struct {
		name    string
		tasks   []swarmTypes.Task
		err     error
		want    []string
		wantErr bool
	}{
		{
			name:  "stopped task of the image",
			tasks: []swarmTypes.Task{task("example.com/app:1@"+digestA, swarmTypes.TaskStateShutdown)},
		},
		{
			name:  "running task of another image",
			tasks: []swarmTypes.Task{task("example.com/app:2@"+digestB, swarmTypes.TaskStateRunning)},
		},
		{
			name: "running task of the image under another tag",
			tasks: []swarmTypes.Task{
				task("example.com/app:1@"+digestA, swarmTypes.TaskStateRunning),
				task("example.com/app:old@"+digestA, swarmTypes.TaskStateRunning),
				{Status: swarmTypes.TaskStatus{State: swarmTypes.TaskStateRunning}},
			},
			want: []string{refA},
		},
		{
			name:    "task list fails",
			err:     errors.New("boom"),
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			apiClient := &imageTasksTestClient{tasks: tc.tasks, err: tc.err}

			got, err := waitForImageTasksStopped(t.Context(), apiClient, []string{refA}, 50*time.Millisecond)
			if (err != nil) != tc.wantErr {
				t.Fatalf("waitForImageTasksStopped() error = %v, wantErr %v", err, tc.wantErr)
			}

			if !slices.Equal(got, tc.want) {
				t.Errorf("waitForImageTasksStopped() = %v, want %v", got, tc.want)
			}
		})
	}
}

type deferredConfigsTestClient struct {
	client.APIClient
	configs   map[string]swarmTypes.Config
	removeErr error
	nextID    int
}

func (c *deferredConfigsTestClient) ConfigList(context.Context, client.ConfigListOptions) (client.ConfigListResult, error) {
	return client.ConfigListResult{Items: slices.Collect(maps.Values(c.configs))}, nil
}

func (c *deferredConfigsTestClient) ConfigCreate(_ context.Context, options client.ConfigCreateOptions) (client.ConfigCreateResult, error) {
	for _, cfg := range c.configs {
		if cfg.Spec.Name == options.Spec.Name {
			return client.ConfigCreateResult{}, errdefs.ErrConflict
		}
	}

	c.nextID++
	id := "config-" + strconv.Itoa(c.nextID)
	c.configs[id] = swarmTypes.Config{ID: id, Spec: options.Spec}

	return client.ConfigCreateResult{ID: id}, nil
}

func (c *deferredConfigsTestClient) ConfigRemove(_ context.Context, id string, _ client.ConfigRemoveOptions) (client.ConfigRemoveResult, error) {
	if c.removeErr != nil {
		return client.ConfigRemoveResult{}, c.removeErr
	}

	delete(c.configs, id)

	return client.ConfigRemoveResult{}, nil
}

func TestDeferredImageRemovals(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	logger := slog.New(slog.DiscardHandler)
	refA := "example.com/app@sha256:" + strings.Repeat("a", 64)
	refB := "example.com/app@sha256:" + strings.Repeat("b", 64)

	apiClient := &deferredConfigsTestClient{configs: map[string]swarmTypes.Config{}}

	if err := deferImageRemovals(ctx, apiClient, []string{refA, refB}, nil); err != nil {
		t.Fatalf("deferImageRemovals() error = %v", err)
	}

	// A concurrent run that deferred the same image first is not an error.
	if err := deferImageRemovals(ctx, apiClient, []string{refA}, nil); err != nil {
		t.Fatalf("deferImageRemovals() of an already deferred image error = %v", err)
	}

	deferred, err := deferredImageRemovals(ctx, apiClient)
	if err != nil {
		t.Fatalf("deferredImageRemovals() error = %v", err)
	}

	if got := slices.Sorted(maps.Keys(deferred)); !slices.Equal(got, []string{refA, refB}) {
		t.Fatalf("deferred images = %v, want %v", got, []string{refA, refB})
	}

	for _, cfg := range apiClient.configs {
		if cfg.Spec.Labels[DocoCDLabels.Metadata.Manager] == "" || len(cfg.Spec.Name) > 64 {
			t.Errorf("config %q lacks the manager label or exceeds the name limit", cfg.Spec.Name)
		}
	}

	apiClient.removeErr = errdefs.ErrNotFound
	forgetDeferredImageRemovals(ctx, apiClient, deferred, func(string) bool { return true }, logger)
	apiClient.removeErr = nil

	forgetDeferredImageRemovals(ctx, apiClient, deferred, func(ref string) bool { return ref == refA }, logger)

	deferred, err = deferredImageRemovals(ctx, apiClient)
	if err != nil {
		t.Fatalf("deferredImageRemovals() error = %v", err)
	}

	if got := slices.Collect(maps.Keys(deferred)); !slices.Equal(got, []string{refB}) {
		t.Errorf("deferred images after forgetting %s = %v, want [%s]", refA, got, refB)
	}
}
