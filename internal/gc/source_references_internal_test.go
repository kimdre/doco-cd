package gc

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker"
)

const (
	testHostDataRoot      = "/srv/doco-cd/data"
	testContainerDataRoot = "/data"
)

// sourceRefsTestClient is a sourceReferenceClient fake returning canned objects. Volumes missing from volumes are
// reported as not found.
type sourceRefsTestClient struct {
	client.APIClient

	containers []container.Summary
	services   []swarm.Service
	tasks      []swarm.Task
	volumes    map[string]volume.Volume

	containerErr error
	serviceErr   error
	taskErr      error
	volumeErr    error

	inspected []string
}

func (c *sourceRefsTestClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: c.containers}, c.containerErr
}

func (c *sourceRefsTestClient) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	return client.ServiceListResult{Items: c.services}, c.serviceErr
}

func (c *sourceRefsTestClient) TaskList(context.Context, client.TaskListOptions) (client.TaskListResult, error) {
	return client.TaskListResult{Items: c.tasks}, c.taskErr
}

func (c *sourceRefsTestClient) VolumeInspect(_ context.Context, name string, _ client.VolumeInspectOptions) (client.VolumeInspectResult, error) {
	c.inspected = append(c.inspected, name)

	if c.volumeErr != nil {
		return client.VolumeInspectResult{}, c.volumeErr
	}

	v, ok := c.volumes[name]
	if !ok {
		return client.VolumeInspectResult{}, errdefs.ErrNotFound
	}

	return client.VolumeInspectResult{Volume: v}, nil
}

func bindVolume(device string) volume.Volume {
	return volume.Volume{
		Driver:     "local",
		Mountpoint: "/var/lib/docker/volumes/x/_data",
		Options:    map[string]string{"type": "none", "o": "bind", "device": device},
	}
}

func collectTestReferences(t *testing.T, apiClient *sourceRefsTestClient, swarmMode bool) *sourceReferences {
	t.Helper()

	refs := newSourceReferences(testHostDataRoot, testContainerDataRoot)
	if err := refs.addContext(context.Background(), apiClient, swarmMode); err != nil {
		t.Fatalf("addContext() error = %v", err)
	}

	return refs
}

func assertReferenced(t *testing.T, refs *sourceReferences, referenced []string, unreferenced []string) {
	t.Helper()

	for _, store := range referenced {
		if by := refs.referencedBy(store); by == "" {
			t.Errorf("store %s is not referenced, want referenced (paths %v, sources %v)", store, refs.paths, refs.repositories)
		}
	}

	for _, store := range unreferenced {
		if by := refs.referencedBy(store); by != "" {
			t.Errorf("store %s is referenced by %s, want unreferenced", store, by)
		}
	}
}

func TestSourceReferences_ContainerMounts(t *testing.T) {
	t.Parallel()

	apiClient := &sourceRefsTestClient{
		containers: []container.Summary{
			{
				// A stopped container without doco-cd labels still holds its bind mount.
				ID:    "stopped",
				State: container.StateExited,
				Mounts: []container.MountPoint{
					{Type: mount.TypeBind, Source: testHostDataRoot + "/github.com/a/bind/artifacts/abc/config"},
				},
			},
			{
				ID: "volumes",
				Mounts: []container.MountPoint{
					// A volume of the local driver binding a directory of a store.
					{Type: mount.TypeVolume, Name: "localbind", Driver: "local", Source: "/var/lib/docker/volumes/localbind/_data"},
					// A volume of a plugin is only known by its storage location.
					{Type: mount.TypeVolume, Name: "plugin", Driver: "nfs", Source: testHostDataRoot + "/github.com/a/plugin"},
					// A volume that was removed in the meantime holds nothing.
					{Type: mount.TypeVolume, Name: "gone", Driver: "local"},
				},
			},
			{
				ID: "same-volume",
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "localbind", Driver: "local"},
				},
			},
			{
				// doco-cd itself, and agents mounting the host root, reference no particular store.
				ID: "doco-cd",
				Mounts: []container.MountPoint{
					{Type: mount.TypeBind, Source: testHostDataRoot},
					{Type: mount.TypeBind, Source: "/"},
					{Type: mount.TypeBind, Source: "/srv"},
					{Type: mount.TypeBind, Source: "/opt/elsewhere/github.com/a/unused"},
					{Type: mount.TypeBind, Source: "relative/github.com/a/unused"},
				},
			},
			{
				// Paths recorded from inside the doco-cd container use the container's data directory.
				ID: "container-path",
				Mounts: []container.MountPoint{
					{Type: mount.TypeBind, Source: testContainerDataRoot + "/github.com/a/inner/live"},
				},
			},
		},
		volumes: map[string]volume.Volume{
			"localbind": bindVolume(testHostDataRoot + "/github.com/a/volume/artifacts"),
		},
	}

	refs := collectTestReferences(t, apiClient, false)

	assertReferenced(t, refs,
		[]string{"github.com/a/bind", "github.com/a/volume", "github.com/a/plugin", "github.com/a/inner"},
		[]string{"github.com/a/unused", "github.com/a/other", "github.com/b/bind"},
	)

	if refs.paths.Contains(".") {
		t.Errorf("paths contain the data directory itself: %v", refs.paths)
	}

	if want := []string{"localbind", "gone"}; !slices.Equal(apiClient.inspected, want) {
		t.Errorf("inspected volumes = %v, want each local volume inspected once: %v", apiClient.inspected, want)
	}
}

func TestSourceReferences_Labels(t *testing.T) {
	t.Parallel()

	apiClient := &sourceRefsTestClient{
		containers: []container.Summary{
			{
				// A stack deployed from github.com/c/repo whose compose files come from another store: the cache
				// identity of a store does not tell which deployments use it.
				ID: "compose",
				Labels: map[string]string{
					docker.DocoCDLabels.Source.URL:            "https://github.com/c/repo.git",
					docker.DocoCDLabels.Deployment.WorkingDir: testContainerDataRoot + "/github.com/b/working/artifacts/abc",
					api.WorkingDirLabel:                       testHostDataRoot + "/github.com/b/compose-working",
					api.ConfigFilesLabel: testHostDataRoot + "/github.com/b/files/artifacts/abc/compose.yaml, " +
						testHostDataRoot + "/github.com/b/override/compose.override.yaml",
					docker.DocoCDLabels.Source.ConfigWorkingDir: testHostDataRoot + "/github.com/b/config/artifacts/def",
				},
			},
			{
				ID: "named",
				Labels: map[string]string{
					docker.DocoCDLabels.Source.Name: "d/named",
				},
			},
		},
	}

	refs := collectTestReferences(t, apiClient, false)

	assertReferenced(t, refs,
		[]string{
			"github.com/c/repo",
			"github.com/b/working",
			"github.com/b/compose-working",
			"github.com/b/files",
			"github.com/b/override",
			"github.com/b/config",
			"github.com/d/named",
		},
		[]string{"github.com/c/other", "github.com/b/unused", "github.com/d/unnamed"},
	)
}

func TestSourceReferences_Swarm(t *testing.T) {
	t.Parallel()

	newClient := func() *sourceRefsTestClient {
		return &sourceRefsTestClient{
			services: []swarm.Service{
				{
					Spec: swarm.ServiceSpec{
						Annotations: swarm.Annotations{
							Name:   "web",
							Labels: map[string]string{docker.DocoCDLabels.Source.Name: "e/service"},
						},
						TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
							Mounts: []mount.Mount{
								// Created by every node with these options.
								{Type: mount.TypeVolume, Source: "driver", VolumeOptions: &mount.VolumeOptions{
									DriverConfig: &mount.Driver{Options: map[string]string{
										"o": "rbind", "device": testHostDataRoot + "/github.com/e/driver",
									}},
								}},
								// The options of other drivers are not host paths.
								{Type: mount.TypeVolume, Source: "remote", VolumeOptions: &mount.VolumeOptions{
									DriverConfig: &mount.Driver{Name: "nfs", Options: map[string]string{
										"o": "bind", "device": ":/export",
									}},
								}},
								// An existing volume is used as is.
								{Type: mount.TypeVolume, Source: "existing"},
							},
						}},
					},
					// The specification being rolled back from or to still has running tasks.
					PreviousSpec: &swarm.ServiceSpec{
						TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
							Mounts: []mount.Mount{
								{Type: mount.TypeBind, Source: testHostDataRoot + "/github.com/e/previous/artifacts/abc"},
							},
						}},
					},
				},
			},
			tasks: []swarm.Task{
				{
					ID:     "old-task",
					Status: swarm.TaskStatus{State: swarm.TaskStateShutdown},
					Spec: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
						Labels: map[string]string{api.WorkingDirLabel: testHostDataRoot + "/github.com/e/task-label"},
						Mounts: []mount.Mount{
							{Type: mount.TypeBind, Source: testHostDataRoot + "/github.com/e/task"},
						},
					}},
				},
				{ID: "no-container-spec"},
			},
			volumes: map[string]volume.Volume{
				"existing": bindVolume(testHostDataRoot + "/github.com/e/existing"),
			},
		}
	}

	swarmStores := []string{
		"github.com/e/service",
		"github.com/e/driver",
		"github.com/e/existing",
		"github.com/e/previous",
		"github.com/e/task",
		"github.com/e/task-label",
	}

	refs := collectTestReferences(t, newClient(), true)
	assertReferenced(t, refs, swarmStores, []string{"github.com/e/remote", "github.com/e/unused"})

	// Without Swarm mode, services and tasks are not listed.
	refs = collectTestReferences(t, newClient(), false)
	assertReferenced(t, refs, nil, swarmStores)
}

func TestSourceReferences_FailsClosed(t *testing.T) {
	t.Parallel()

	errDocker := errors.New("docker unavailable")

	bindMountOf := func(name string) []container.Summary {
		return []container.Summary{{ID: "c", Mounts: []container.MountPoint{{Type: mount.TypeVolume, Name: name, Driver: "local"}}}}
	}

	tests := []struct {
		name      string
		client    *sourceRefsTestClient
		swarmMode bool
	}{
		{name: "containers cannot be listed", client: &sourceRefsTestClient{containerErr: errDocker}},
		{name: "services cannot be listed", client: &sourceRefsTestClient{serviceErr: errDocker}, swarmMode: true},
		{name: "tasks cannot be listed", client: &sourceRefsTestClient{taskErr: errDocker}, swarmMode: true},
		{
			name:   "volume cannot be inspected",
			client: &sourceRefsTestClient{containers: bindMountOf("vol"), volumeErr: errDocker},
		},
		{
			name: "volume binds a relative path",
			client: &sourceRefsTestClient{
				containers: bindMountOf("vol"),
				volumes:    map[string]volume.Volume{"vol": bindVolume("relative/path")},
			},
		},
		{
			name: "swarm volume binds no path",
			client: &sourceRefsTestClient{tasks: []swarm.Task{{Spec: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
				Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: "vol", VolumeOptions: &mount.VolumeOptions{
					DriverConfig: &mount.Driver{Name: "local", Options: map[string]string{"o": "bind"}},
				}}},
			}}}}},
			swarmMode: true,
		},
		{
			name: "swarm volume cannot be inspected",
			client: &sourceRefsTestClient{
				tasks: []swarm.Task{{Spec: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
					Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: "vol"}},
				}}}},
				volumeErr: errDocker,
			},
			swarmMode: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			refs := newSourceReferences(testHostDataRoot, testContainerDataRoot)
			if err := refs.addContext(context.Background(), tt.client, tt.swarmMode); err == nil {
				t.Fatal("addContext() error = nil, want error")
			}
		})
	}
}

func TestSourceReferencesOf(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	first := &sourceRefsTestClient{containers: []container.Summary{{
		Mounts: []container.MountPoint{{Type: mount.TypeBind, Source: testHostDataRoot + "/github.com/f/first"}},
	}}}
	second := &sourceRefsTestClient{containers: []container.Summary{{
		Mounts: []container.MountPoint{{Type: mount.TypeBind, Source: testHostDataRoot + "/github.com/f/second"}},
	}}}

	results := []docker.ContextClientResult{
		{ContextClient: docker.ContextClient{Cli: liveTestCli{apiClient: first}}},
		{ContextClient: docker.ContextClient{Name: "remote", Cli: liveTestCli{apiClient: second}}},
	}

	refs, err := sourceReferencesOf(ctx, results, testHostDataRoot, testContainerDataRoot)
	if err != nil {
		t.Fatalf("sourceReferencesOf() error = %v", err)
	}

	assertReferenced(t, refs, []string{"github.com/f/first", "github.com/f/second"}, []string{"github.com/f/third"})

	// A context that cannot be reached could use any store.
	unavailable := append(slices.Clone(results), docker.ContextClientResult{
		ContextClient: docker.ContextClient{Name: "down"},
		Err:           errors.New("connection refused"),
	})
	if _, err := sourceReferencesOf(ctx, unavailable, testHostDataRoot, testContainerDataRoot); err == nil {
		t.Error("sourceReferencesOf() with an unavailable context: error = nil, want error")
	}

	withoutClient := []docker.ContextClientResult{{ContextClient: docker.ContextClient{Name: "broken"}}}
	if _, err := sourceReferencesOf(ctx, withoutClient, testHostDataRoot, testContainerDataRoot); err == nil {
		t.Error("sourceReferencesOf() with a context without client: error = nil, want error")
	}

	if _, err := collectSourceReferences(ctx, nil, testHostDataRoot, testContainerDataRoot); err == nil {
		t.Error("collectSourceReferences() without registry: error = nil, want error")
	}
}

func TestSourceReferences_ReferencedBy(t *testing.T) {
	t.Parallel()

	refs := newSourceReferences(testContainerDataRoot)
	refs.addPath(testContainerDataRoot + "/github.com/g/app.evicting/artifacts")
	refs.addPath(testContainerDataRoot + "/gitlab.com/group")
	refs.addPath(testContainerDataRoot + "/github.com/g/store/artifacts/abc/compose.yaml")
	refs.repositories.Add("h/repo")

	tests := []struct {
		store string
		want  string
	}{
		// A path in a store references it.
		{store: "github.com/g/store", want: "path github.com/g/store/artifacts/abc/compose.yaml"},
		// A path above a store references every store below it.
		{store: "gitlab.com/group/sub/repo", want: "path gitlab.com/group"},
		// Path components are compared as a whole.
		{store: "github.com/g/app", want: ""},
		{store: "github.com/g/sto", want: ""},
		{store: "gitlab.com/groupie/repo", want: ""},
		// Sources match the store of their repository regardless of its host.
		{store: "github.com/h/repo", want: "source h/repo"},
		{store: "github.com/h/repository", want: ""},
	}

	for _, tt := range tests {
		if got := refs.referencedBy(tt.store); got != tt.want {
			t.Errorf("referencedBy(%q) = %q, want %q", tt.store, got, tt.want)
		}
	}
}
