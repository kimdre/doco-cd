package docker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	swarmInternal "github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/source/store"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// swarmPinningTestClient is a client.APIClient fake returning canned services.
type swarmPinningTestClient struct {
	client.APIClient
	services []swarm.Service
}

func (c *swarmPinningTestClient) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	return client.ServiceListResult{Items: c.services}, nil
}

func TestPinSwarmServiceMounts(t *testing.T) {
	t.Parallel()

	files := map[string]string{
		"static/index.html": "static",
		"conf/app.conf":     "conf",
	}

	setup := func(t *testing.T, revisionFiles map[string]map[string]string) string {
		t.Helper()

		base := t.TempDir()
		for revision, revFiles := range revisionFiles {
			writePinningTestFiles(t, filepath.Join(base, store.ArtifactsSubdir, revision), revFiles)
		}

		return base
	}

	bind := func(source, target string) composetypes.ServiceVolumeConfig {
		return composetypes.ServiceVolumeConfig{Type: string(mount.TypeBind), Source: source, Target: target}
	}

	mountBind := func(source, target string) mount.Mount {
		return mount.Mount{Type: mount.TypeBind, Source: source, Target: target}
	}

	t.Run("unchanged mounts are pinned", func(t *testing.T) {
		t.Parallel()

		base := setup(t, map[string]map[string]string{"old": files, "older": files, "new": files})
		root := func(revision string) string { return filepath.Join(base, store.ArtifactsSubdir, revision) }

		volumes := []composetypes.ServiceVolumeConfig{
			bind(filepath.Join(root("new"), "static"), "/static"),
			bind(filepath.Join(root("new"), "conf", "app.conf"), "/etc/app.conf"),
			bind("/etc/hosts", "/etc/hosts"),
			{Type: string(mount.TypeVolume), Source: "data", Target: "/data"},
		}
		current := []mount.Mount{
			mountBind(filepath.Join(root("older"), "static"), "/static"),
			mountBind(filepath.Join(root("old"), "conf", "app.conf"), "/etc/app.conf"),
			mountBind("/etc/hosts", "/etc/hosts"),
			{Type: mount.TypeVolume, Source: "stack_data", Target: "/data"},
		}

		pinned, revisions, ok, err := pinSwarmServiceMounts(volumes, current, base, root("new"))
		if err != nil || !ok {
			t.Fatalf("pinSwarmServiceMounts() = %v, %v, want pinned", ok, err)
		}

		if got, want := pinned[0].Source, filepath.Join(root("older"), "static"); got != want {
			t.Errorf("static source = %q, want %q", got, want)
		}

		if got, want := pinned[1].Source, filepath.Join(root("old"), "conf", "app.conf"); got != want {
			t.Errorf("conf source = %q, want %q", got, want)
		}

		if got := pinned[2].Source; got != "/etc/hosts" {
			t.Errorf("bind mount outside of the artifact = %q, want it unchanged", got)
		}

		if got := pinned[3].Source; got != "data" {
			t.Errorf("volume source = %q, want it unchanged", got)
		}

		if got, want := FormatPinnedRevisions(revisions), "old,older"; got != want {
			t.Errorf("revisions = %q, want %q", got, want)
		}

		if got, want := volumes[0].Source, filepath.Join(root("new"), "static"); got != want {
			t.Errorf("original volumes were modified: %q, want %q", got, want)
		}
	})

	testCases := []struct {
		name     string
		newFiles map[string]string
		current  func(root func(string) string) []mount.Mount
	}{
		{
			name:     "changed content",
			newFiles: map[string]string{"static/index.html": "changed", "conf/app.conf": "conf"},
		},
		{
			name:     "mounted from a different path",
			newFiles: files,
			current: func(root func(string) string) []mount.Mount {
				return []mount.Mount{mountBind(filepath.Join(root("old"), "conf"), "/static")}
			},
		},
		{
			name:     "mounted from outside of the store",
			newFiles: files,
			current: func(func(string) string) []mount.Mount {
				return []mount.Mount{mountBind("/srv/static", "/static")}
			},
		},
		{
			name:     "mounted from a removed artifact",
			newFiles: files,
			current: func(root func(string) string) []mount.Mount {
				return []mount.Mount{mountBind(filepath.Join(root("removed"), "static"), "/static")}
			},
		},
		{
			name:     "not mounted yet",
			newFiles: files,
			current:  func(func(string) string) []mount.Mount { return nil },
		},
		{
			name:     "volume at target",
			newFiles: files,
			current: func(func(string) string) []mount.Mount {
				return []mount.Mount{{Type: mount.TypeVolume, Source: "stack_static", Target: "/static"}}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name+" is not pinned", func(t *testing.T) {
			t.Parallel()

			base := setup(t, map[string]map[string]string{"old": files, "new": tc.newFiles})
			root := func(revision string) string { return filepath.Join(base, store.ArtifactsSubdir, revision) }

			current := []mount.Mount{mountBind(filepath.Join(root("old"), "static"), "/static")}
			if tc.current != nil {
				current = tc.current(root)
			}

			_, _, ok, err := pinSwarmServiceMounts(
				[]composetypes.ServiceVolumeConfig{bind(filepath.Join(root("new"), "static"), "/static")},
				current, base, root("new"))
			if err != nil || ok {
				t.Fatalf("pinSwarmServiceMounts() = %v, %v, want not pinned", ok, err)
			}
		})
	}

	for name, tc := range map[string]struct {
		newContent string
		want       bool
	}{
		"symlinked source with unchanged target is pinned":   {newContent: "prod", want: true},
		"symlinked source with changed target is not pinned": {newContent: "changed", want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			base := setup(t, map[string]map[string]string{
				"old": {"config/prod.conf": "prod"},
				"new": {"config/prod.conf": tc.newContent},
			})
			root := func(revision string) string { return filepath.Join(base, store.ArtifactsSubdir, revision) }

			for _, revision := range []string{"old", "new"} {
				if err := os.Symlink("config/prod.conf", filepath.Join(root(revision), "app.conf")); err != nil {
					t.Fatal(err)
				}
			}

			_, _, ok, err := pinSwarmServiceMounts(
				[]composetypes.ServiceVolumeConfig{bind(filepath.Join(root("new"), "app.conf"), "/etc/app.conf")},
				[]mount.Mount{mountBind(filepath.Join(root("old"), "app.conf"), "/etc/app.conf")},
				base, root("new"))
			if err != nil || ok != tc.want {
				t.Fatalf("pinSwarmServiceMounts() = %v, %v, want %v", ok, err, tc.want)
			}
		})
	}

	t.Run("service without repository bind mounts is not pinned", func(t *testing.T) {
		t.Parallel()

		base := setup(t, map[string]map[string]string{"old": files, "new": files})

		_, _, ok, err := pinSwarmServiceMounts(
			[]composetypes.ServiceVolumeConfig{bind("/etc/hosts", "/etc/hosts")},
			[]mount.Mount{mountBind("/etc/hosts", "/etc/hosts")},
			base, filepath.Join(base, store.ArtifactsSubdir, "new"))
		if err != nil || ok {
			t.Fatalf("pinSwarmServiceMounts() = %v, %v, want not pinned", ok, err)
		}
	})
}

func TestPinUnchangedSwarmServices(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	oldRoot := filepath.Join(base, store.ArtifactsSubdir, "sha256-old")
	newRoot := filepath.Join(base, store.ArtifactsSubdir, "sha256-new")

	for _, root := range []string{oldRoot, newRoot} {
		writePinningTestFiles(t, root, map[string]string{"static/index.html": "static"})
	}

	fake := &swarmPinningTestClient{services: []swarm.Service{{
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{Name: "stack_web"},
			TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Mounts: []mount.Mount{
				{Type: mount.TypeBind, Source: filepath.Join(oldRoot, "static"), Target: "/static"},
			}}},
		},
	}}}

	newStack := func() *composetypes.Config {
		volumes := []composetypes.ServiceVolumeConfig{
			{Type: string(mount.TypeBind), Source: filepath.Join(newRoot, "static"), Target: "/static"},
		}

		return &composetypes.Config{Services: []composetypes.ServiceConfig{
			{Name: "web", Volumes: volumes, Deploy: composetypes.DeployConfig{Labels: map[string]string{"a": "b"}}},
			{Name: "new", Volumes: volumes},
		}}
	}

	stack := newStack()

	if err := pinUnchangedSwarmServices(t.Context(), fake, stack, "stack", newRoot, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	web := stack.Services[0]
	if got, want := web.Volumes[0].Source, filepath.Join(oldRoot, "static"); got != want {
		t.Errorf("web bind mount source = %q, want %q", got, want)
	}

	if got, want := web.Deploy.Labels[DocoCDLabels.Deployment.PinnedRevisions], "sha256:old"; got != want {
		t.Errorf("web pinned revisions label = %q, want %q", got, want)
	}

	if got := web.Labels[DocoCDLabels.Deployment.PinnedRevisions]; got != "" {
		t.Errorf("pinned revisions must not be set as container label, got %q", got)
	}

	newService := stack.Services[1]
	if got, want := newService.Volumes[0].Source, filepath.Join(newRoot, "static"); got != want {
		t.Errorf("new service bind mount source = %q, want %q", got, want)
	}

	if _, ok := newService.Deploy.Labels[DocoCDLabels.Deployment.PinnedRevisions]; ok {
		t.Errorf("new service must not be pinned")
	}

	stack = newStack()

	if err := pinUnchangedSwarmServices(t.Context(), fake, stack, "other", newRoot, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	if got, want := stack.Services[0].Volumes[0].Source, filepath.Join(newRoot, "static"); got != want {
		t.Errorf("service of another stack was pinned: %q, want %q", got, want)
	}
}

// runningServiceTaskIDs returns the IDs of the running tasks of a stack by service name.
func runningServiceTaskIDs(ctx context.Context, t *testing.T, cli client.APIClient, stackName string) map[string][]string {
	t.Helper()

	services, err := swarmInternal.GetStackServices(ctx, cli, stackName)
	if err != nil {
		t.Fatalf("failed to list services of stack %s: %v", stackName, err)
	}

	names := make(map[string]string, len(services))
	for _, service := range services {
		names[service.ID] = strings.TrimPrefix(service.Spec.Name, stackName+"_")
	}

	tasks, err := cli.TaskList(ctx, client.TaskListOptions{
		Filters: make(client.Filters).Add("label", swarmInternal.StackNamespaceLabel+"="+stackName),
	})
	if err != nil {
		t.Fatalf("failed to list tasks of stack %s: %v", stackName, err)
	}

	ids := make(map[string][]string)

	for _, task := range tasks.Items {
		if task.DesiredState == swarm.TaskStateRunning {
			ids[names[task.ServiceID]] = append(ids[names[task.ServiceID]], task.ID)
		}
	}

	for _, serviceIDs := range ids {
		slices.Sort(serviceIDs)
	}

	return ids
}

// TestDeploySwarmStackKeepsUnchangedBindMounts verifies that deploying a new revision only
// updates the services whose bind-mounted repository files changed, although every revision
// is deployed from its own artifact directory.
func TestDeploySwarmStackKeepsUnchangedBindMounts(t *testing.T) {
	ctx := t.Context()

	dockerCli, err := CreateDockerCli(true)
	if err != nil {
		t.Fatalf("failed to create docker cli: %v", err)
	}

	if !resolveTestSwarmMode(ctx, t, dockerCli.Client()) {
		t.Skip("Swarm mode is not enabled, skipping test")
	}

	stackName := test.ConvertTestName(t.Name())
	storeBase := t.TempDir()

	const composeYAML = `
services:
  unchanged:
    image: alpine:3.21
    command: ["sleep", "infinity"]
    stop_grace_period: 1s
    environment:
      EXTRA: "%s"
    volumes:
      - ./static:/static:ro
  changed:
    image: alpine:3.21
    command: ["sleep", "infinity"]
    stop_grace_period: 1s
    volumes:
      - ./conf:/conf:ro
`

	revisionFiles := func(extra, conf string) map[string]string {
		return map[string]string{
			"compose.yaml": fmt.Sprintf(composeYAML, extra), "static/index.html": "static", "conf/app.conf": conf,
		}
	}

	deployConfig := &deploy.Config{Name: stackName, Timeout: 120}
	payload := &webhook.ParsedPayload{CommitSHA: plumbing.ZeroHash, FullName: "kimdre/doco-cd_tests"}

	t.Cleanup(func() {
		if err := RemoveSwarmStack(context.Background(), dockerCli, stackName); err != nil {
			t.Logf("failed to remove swarm stack: %v", err)
		}
	})

	deployRevision := func(revision string, files map[string]string) string {
		t.Helper()

		root := filepath.Join(storeBase, store.ArtifactsSubdir, revision)
		writePinningTestFiles(t, root, files)

		project, err := LoadCompose(ctx, nil, root, root, stackName, []string{filepath.Join(root, "compose.yaml")},
			nil, nil, map[string]string{}, ComposeLoadOptions{})
		if err != nil {
			t.Fatalf("failed to load compose project of %s: %v", revision, err)
		}

		cfg, opts, err := LoadSwarmStack(dockerCli, project, deployConfig, root)
		if err != nil {
			t.Fatalf("failed to load swarm stack of %s: %v", revision, err)
		}

		addSwarmServiceLabels(cfg, project, deployConfig, payload, "", root, "dev",
			time.Now().UTC().Format(time.RFC3339), revision, "hash-"+revision)

		if err = pinUnchangedSwarmServices(ctx, dockerCli.Client(), cfg, stackName, root, slog.New(slog.DiscardHandler)); err != nil {
			t.Fatalf("failed to pin services of %s: %v", revision, err)
		}

		err = retry.New(retry.Attempts(5), retry.Delay(2*time.Second), retry.Context(ctx)).Do(func() error {
			return DeploySwarmStack(ctx, dockerCli, cfg, opts)
		})
		if err != nil {
			t.Fatalf("failed to deploy %s: %v", revision, err)
		}

		return root
	}

	serviceSpec := func(name string) swarm.ServiceSpec {
		t.Helper()

		services, err := swarmInternal.GetStackServices(ctx, dockerCli.Client(), stackName)
		if err != nil {
			t.Fatal(err)
		}

		for _, service := range services {
			if service.Spec.Name == stackName+"_"+name {
				return service.Spec
			}
		}

		t.Fatalf("service %s not found", name)

		return swarm.ServiceSpec{}
	}

	root1 := deployRevision("rev1", revisionFiles("1", "v1"))
	first := runningServiceTaskIDs(ctx, t, dockerCli.Client(), stackName)

	deployRevision("rev2", revisionFiles("1", "v2"))

	second := runningServiceTaskIDs(ctx, t, dockerCli.Client(), stackName)

	if len(first["unchanged"]) == 0 || !slices.Equal(first["unchanged"], second["unchanged"]) {
		t.Errorf("service with unchanged bind mounts was updated: %v -> %v", first["unchanged"], second["unchanged"])
	}

	if slices.Equal(first["changed"], second["changed"]) {
		t.Errorf("service with a changed bind mount was not updated")
	}

	deployRevision("rev3", revisionFiles("2", "v2"))

	third := runningServiceTaskIDs(ctx, t, dockerCli.Client(), stackName)

	if slices.Equal(second["unchanged"], third["unchanged"]) {
		t.Fatalf("service with a changed config was not updated")
	}

	if !slices.Equal(second["changed"], third["changed"]) {
		t.Errorf("service with bind mounts unchanged since the previous revision was updated")
	}

	spec := serviceSpec("unchanged")

	if got, want := spec.TaskTemplate.ContainerSpec.Mounts[0].Source, filepath.Join(root1, "static"); got != want {
		t.Errorf("updated service mounts %q, want the pinned artifact %q", got, want)
	}

	if got := spec.Labels[DocoCDLabels.Deployment.PinnedRevisions]; got != "rev1" {
		t.Errorf("updated service pinned revisions label = %q, want %q", got, "rev1")
	}
}
