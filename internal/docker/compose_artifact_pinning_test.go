package docker

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/types/duration"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/source/store"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func writePinningTestFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(root, name)

		if err := os.MkdirAll(filepath.Dir(path), filesystem.PermDir); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), filesystem.PermPublic); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArtifactRootFromWorkingDir(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		workingDir string
		configured string
		want       string
	}{
		{name: "repository root", workingDir: "/data/repo/artifacts/abc", configured: "", want: "/data/repo/artifacts/abc"},
		{name: "dot", workingDir: "/data/repo/artifacts/abc", configured: ".", want: "/data/repo/artifacts/abc"},
		{name: "sub directory", workingDir: "/data/repo/artifacts/abc/deploy/app", configured: "deploy/app", want: "/data/repo/artifacts/abc"},
		{name: "relative prefix", workingDir: "/data/repo/artifacts/abc/deploy", configured: "./deploy/", want: "/data/repo/artifacts/abc"},
		{name: "sub directory named artifacts", workingDir: "/data/repo/artifacts/abc/artifacts/x", configured: "artifacts/x", want: "/data/repo/artifacts/abc"},
		{name: "mismatching sub directory", workingDir: "/data/repo/artifacts/abc/deploy", configured: "other", want: ""},
		{name: "legacy layout", workingDir: "/data/repo/deploy", configured: "deploy", want: ""},
		{name: "empty", workingDir: "", configured: "", want: ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := artifactRootFromWorkingDir(tc.workingDir, tc.configured); got != tc.want {
				t.Fatalf("artifactRootFromWorkingDir(%q, %q) = %q, want %q", tc.workingDir, tc.configured, got, tc.want)
			}
		})
	}
}

func TestContainersArtifactRoot(t *testing.T) {
	t.Parallel()

	const storeBase = "/data/repo"

	withLabels := func(labels map[string]string) container.Summary {
		return container.Summary{Labels: labels}
	}

	testCases := []struct {
		name         string
		containers   []container.Summary
		wantRoot     string
		wantRevision store.Revision
		wantOK       bool
	}{
		{
			name: "working directory",
			containers: []container.Summary{
				withLabels(map[string]string{DocoCDLabels.Deployment.WorkingDir: "/data/repo/artifacts/abc/deploy"}),
				withLabels(map[string]string{DocoCDLabels.Deployment.WorkingDir: "/data/repo/artifacts/abc/deploy"}),
			},
			wantRoot:     "/data/repo/artifacts/abc",
			wantRevision: "abc",
			wantOK:       true,
		},
		{
			name: "pinned revision takes precedence",
			containers: []container.Summary{withLabels(map[string]string{
				DocoCDLabels.Deployment.WorkingDir:      "/data/repo/artifacts/new/deploy",
				DocoCDLabels.Deployment.PinnedRevisions: "sha256:old",
			})},
			wantRoot:     "/data/repo/artifacts/sha256-old",
			wantRevision: "sha256:old",
			wantOK:       true,
		},
		{
			name: "multiple pinned revisions",
			containers: []container.Summary{withLabels(map[string]string{
				DocoCDLabels.Deployment.WorkingDir:      "/data/repo/artifacts/new",
				DocoCDLabels.Deployment.PinnedRevisions: "a,b",
			})},
		},
		{
			name: "containers of different artifacts",
			containers: []container.Summary{
				withLabels(map[string]string{DocoCDLabels.Deployment.WorkingDir: "/data/repo/artifacts/abc"}),
				withLabels(map[string]string{DocoCDLabels.Deployment.WorkingDir: "/data/repo/artifacts/def"}),
			},
		},
		{
			name:       "different store",
			containers: []container.Summary{withLabels(map[string]string{DocoCDLabels.Deployment.WorkingDir: "/data/other/artifacts/abc"})},
		},
		{
			name:       "missing working directory",
			containers: []container.Summary{withLabels(map[string]string{})},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root, revision, ok := containersArtifactRoot(tc.containers, storeBase)
			if root != tc.wantRoot || revision != tc.wantRevision || ok != tc.wantOK {
				t.Fatalf("containersArtifactRoot() = (%q, %q, %v), want (%q, %q, %v)",
					root, revision, ok, tc.wantRoot, tc.wantRevision, tc.wantOK)
			}
		})
	}
}

func TestPinComposeService(t *testing.T) {
	t.Parallel()

	baseFiles := map[string]string{
		"static/index.html": "<h1>hello</h1>",
		"app.env":           "FOO=bar",
		"app.labels":        "com.example=1",
		"watch/file.txt":    "watch",
	}

	newService := func(newRoot string) types.ServiceConfig {
		return types.ServiceConfig{
			Name: "web",
			Volumes: []types.ServiceVolumeConfig{
				{Type: types.VolumeTypeBind, Source: filepath.Join(newRoot, "static"), Target: "/static"},
				{Type: types.VolumeTypeBind, Source: "/etc/hosts", Target: "/host/hosts"},
				{Type: types.VolumeTypeVolume, Source: "data", Target: "/data"},
			},
			EnvFiles:   []types.EnvFile{{Path: filepath.Join(newRoot, "app.env")}},
			LabelFiles: []string{filepath.Join(newRoot, "app.labels")},
			Develop: &types.DevelopConfig{Watch: []types.Trigger{
				{Path: filepath.Join(newRoot, "watch"), Action: types.WatchActionSync, Target: "/watch"},
			}},
		}
	}

	mountedFrom := func(root string) container.Summary {
		return container.Summary{Mounts: []container.MountPoint{
			{Type: mount.TypeBind, Source: filepath.Join(root, "static"), Destination: "/static"},
			{Type: mount.TypeBind, Source: "/etc/hosts", Destination: "/host/hosts"},
			{Type: mount.TypeVolume, Name: "data", Destination: "/data"},
		}}
	}

	setup := func(t *testing.T, newFiles map[string]string) (string, string) {
		t.Helper()

		base := t.TempDir()
		oldRoot := filepath.Join(base, store.ArtifactsSubdir, "old")
		newRoot := filepath.Join(base, store.ArtifactsSubdir, "new")

		writePinningTestFiles(t, oldRoot, baseFiles)
		writePinningTestFiles(t, newRoot, newFiles)

		return oldRoot, newRoot
	}

	t.Run("unchanged files are pinned", func(t *testing.T) {
		t.Parallel()

		oldRoot, newRoot := setup(t, baseFiles)
		service := newService(newRoot)

		pinned, ok, err := pinComposeService(service, []container.Summary{mountedFrom(oldRoot)}, newRoot, oldRoot)
		if err != nil || !ok {
			t.Fatalf("pinComposeService() = %v, %v, want pinned", ok, err)
		}

		if got, want := pinned.Volumes[0].Source, filepath.Join(oldRoot, "static"); got != want {
			t.Errorf("bind mount source = %q, want %q", got, want)
		}

		if got := pinned.Volumes[1].Source; got != "/etc/hosts" {
			t.Errorf("bind mount outside of the artifact = %q, want it unchanged", got)
		}

		if got, want := pinned.EnvFiles[0].Path, filepath.Join(oldRoot, "app.env"); got != want {
			t.Errorf("env file = %q, want %q", got, want)
		}

		if got, want := pinned.LabelFiles[0], filepath.Join(oldRoot, "app.labels"); got != want {
			t.Errorf("label file = %q, want %q", got, want)
		}

		if got, want := pinned.Develop.Watch[0].Path, filepath.Join(oldRoot, "watch"); got != want {
			t.Errorf("watch path = %q, want %q", got, want)
		}

		// The service of the project must not be modified in place.
		if got, want := service.Volumes[0].Source, filepath.Join(newRoot, "static"); got != want {
			t.Errorf("original bind mount source = %q, want %q", got, want)
		}

		if got, want := service.Develop.Watch[0].Path, filepath.Join(newRoot, "watch"); got != want {
			t.Errorf("original watch path = %q, want %q", got, want)
		}
	})

	for name, changedFile := range map[string]string{
		"changed bind mount": "static/index.html",
		"changed env file":   "app.env",
		"changed label file": "app.labels",
		"changed watch path": "watch/file.txt",
	} {
		t.Run(name+" is not pinned", func(t *testing.T) {
			t.Parallel()

			newFiles := make(map[string]string, len(baseFiles))
			maps.Copy(newFiles, baseFiles)

			newFiles[changedFile] += "changed"

			oldRoot, newRoot := setup(t, newFiles)

			_, ok, err := pinComposeService(newService(newRoot), []container.Summary{mountedFrom(oldRoot)}, newRoot, oldRoot)
			if err != nil || ok {
				t.Fatalf("pinComposeService() = %v, %v, want not pinned", ok, err)
			}
		})
	}

	t.Run("container mounting a different source is not pinned", func(t *testing.T) {
		t.Parallel()

		oldRoot, newRoot := setup(t, baseFiles)

		_, ok, err := pinComposeService(newService(newRoot),
			[]container.Summary{mountedFrom(oldRoot), mountedFrom(newRoot)}, newRoot, oldRoot)
		if err != nil || ok {
			t.Fatalf("pinComposeService() = %v, %v, want not pinned", ok, err)
		}
	})

	for name, tc := range map[string]struct {
		newContent string
		want       bool
	}{
		"symlinked bind mount source with unchanged target is pinned":   {newContent: "prod", want: true},
		"symlinked bind mount source with changed target is not pinned": {newContent: "changed", want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			oldRoot, newRoot := setup(t, baseFiles)

			for root, content := range map[string]string{oldRoot: "prod", newRoot: tc.newContent} {
				writePinningTestFiles(t, root, map[string]string{"config/prod.conf": content})

				if err := os.Symlink("config/prod.conf", filepath.Join(root, "app.conf")); err != nil {
					t.Fatal(err)
				}
			}

			service := types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{
				{Type: types.VolumeTypeBind, Source: filepath.Join(newRoot, "app.conf"), Target: "/etc/app.conf"},
			}}
			mounted := container.Summary{Mounts: []container.MountPoint{
				{Type: mount.TypeBind, Source: filepath.Join(oldRoot, "app.conf"), Destination: "/etc/app.conf"},
			}}

			_, ok, err := pinComposeService(service, []container.Summary{mounted}, newRoot, oldRoot)
			if err != nil || ok != tc.want {
				t.Fatalf("pinComposeService() = %v, %v, want %v", ok, err, tc.want)
			}
		})
	}

	t.Run("service without repository paths is not pinned", func(t *testing.T) {
		t.Parallel()

		oldRoot, newRoot := setup(t, baseFiles)
		service := types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{
			{Type: types.VolumeTypeBind, Source: "/etc/hosts", Target: "/host/hosts"},
		}}

		_, ok, err := pinComposeService(service, []container.Summary{mountedFrom(oldRoot)}, newRoot, oldRoot)
		if err != nil || ok {
			t.Fatalf("pinComposeService() = %v, %v, want not pinned", ok, err)
		}
	})
}

// pinningTestClient is a client.APIClient fake returning canned containers.
type pinningTestClient struct {
	client.APIClient
	containers []container.Summary
}

func (c *pinningTestClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: c.containers}, nil
}

func TestPinUnchangedComposeServices(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	oldRoot := filepath.Join(base, store.ArtifactsSubdir, "old")
	newRoot := filepath.Join(base, store.ArtifactsSubdir, "new")

	for _, root := range []string{oldRoot, newRoot} {
		writePinningTestFiles(t, root, map[string]string{"static/index.html": "hello"})
	}

	newProject := func() *types.Project {
		services := types.Services{}

		for _, name := range []string{"web", "forced", "ephemeral"} {
			services[name] = types.ServiceConfig{
				Name:         name,
				Volumes:      []types.ServiceVolumeConfig{{Type: types.VolumeTypeBind, Source: filepath.Join(newRoot, "static"), Target: "/static"}},
				CustomLabels: types.Labels{DocoCDLabels.Deployment.WorkingDir: newRoot},
			}
		}

		return &types.Project{Name: "stack", Services: services}
	}

	serviceContainer := func(service string, labels map[string]string) container.Summary {
		allLabels := map[string]string{
			api.ServiceLabel:                   service,
			DocoCDLabels.Deployment.WorkingDir: oldRoot,
		}

		maps.Copy(allLabels, labels)

		return container.Summary{
			Labels: allLabels,
			Mounts: []container.MountPoint{{Type: mount.TypeBind, Source: filepath.Join(oldRoot, "static"), Destination: "/static"}},
		}
	}

	fake := &pinningTestClient{containers: []container.Summary{
		serviceContainer("web", nil),
		serviceContainer("forced", nil),
		serviceContainer("ephemeral", map[string]string{DocoCDJobLabels.JobEphemeral: "true"}),
	}}

	log := slog.New(slog.DiscardHandler)

	t.Run("diverged", func(t *testing.T) {
		t.Parallel()

		project := newProject()

		if err := pinUnchangedComposeServices(t.Context(), fake, project, newRoot, api.RecreateForce, []string{"forced"}, log); err != nil {
			t.Fatal(err)
		}

		web := project.Services["web"]
		if got, want := web.Volumes[0].Source, filepath.Join(oldRoot, "static"); got != want {
			t.Errorf("web bind mount source = %q, want %q", got, want)
		}

		if got := web.CustomLabels[DocoCDLabels.Deployment.PinnedRevisions]; got != "old" {
			t.Errorf("web pinned revisions label = %q, want %q", got, "old")
		}

		if got := web.CustomLabels[DocoCDLabels.Deployment.WorkingDir]; got != newRoot {
			t.Errorf("web working dir label = %q, want the deployed artifact %q", got, newRoot)
		}

		for _, name := range []string{"forced", "ephemeral"} {
			service := project.Services[name]
			if got, want := service.Volumes[0].Source, filepath.Join(newRoot, "static"); got != want {
				t.Errorf("%s bind mount source = %q, want %q", name, got, want)
			}

			if _, ok := service.CustomLabels[DocoCDLabels.Deployment.PinnedRevisions]; ok {
				t.Errorf("%s must not be pinned", name)
			}
		}
	})

	t.Run("force recreate of all services", func(t *testing.T) {
		t.Parallel()

		project := newProject()

		if err := pinUnchangedComposeServices(t.Context(), fake, project, newRoot, api.RecreateForce, nil, log); err != nil {
			t.Fatal(err)
		}

		for name, service := range project.Services {
			if got, want := service.Volumes[0].Source, filepath.Join(newRoot, "static"); got != want {
				t.Errorf("%s bind mount source = %q, want %q", name, got, want)
			}
		}
	})

	t.Run("not an artifact", func(t *testing.T) {
		t.Parallel()

		project := newProject()

		if err := pinUnchangedComposeServices(t.Context(), fake, project, base, api.RecreateDiverged, nil, log); err != nil {
			t.Fatal(err)
		}

		if got, want := project.Services["web"].Volumes[0].Source, filepath.Join(newRoot, "static"); got != want {
			t.Errorf("web bind mount source = %q, want %q", got, want)
		}
	})
}

// TestDeployComposeKeepsUnchangedServices verifies that deploying a new revision only recreates
// the services whose repository files changed, although every revision is deployed from its own
// artifact directory, see https://github.com/kimdre/doco-cd/issues/1911.
func TestDeployComposeKeepsUnchangedServices(t *testing.T) {
	ctx := t.Context()

	dockerCli, err := CreateDockerCli(true)
	if err != nil {
		t.Fatalf("failed to create docker cli: %v", err)
	}

	if err = VerifySocketConnection(); err != nil {
		t.Skipf("docker is not available: %v", err)
	}

	if resolveTestSwarmMode(ctx, t, dockerCli.Client()) {
		t.Skip("Swarm mode is enabled, skipping test")
	}

	stackName := test.ConvertTestName(t.Name())
	storeBase := t.TempDir()

	const composeYAML = `
services:
  unchanged:
    image: alpine:3.21
    pull_policy: missing
    command: ["sleep", "infinity"]
    stop_grace_period: 1s
    environment:
      EXTRA: "%s"
    env_file: app.env
    volumes:
      - ./static:/static:ro
  changed:
    image: alpine:3.21
    pull_policy: missing
    command: ["sleep", "infinity"]
    stop_grace_period: 1s
    volumes:
      - ./conf:/conf:ro
  generated:
    image: alpine:3.21
    pull_policy: missing
    command: ["sleep", "infinity"]
    stop_grace_period: 1s
    volumes:
      - ./data:/data
`

	revisions := []struct {
		revision string
		files    map[string]string
	}{
		{revision: "rev1", files: map[string]string{
			"compose.yaml": fmt.Sprintf(composeYAML, "1"), "app.env": "FOO=bar", "static/index.html": "static", "conf/app.conf": "v1",
		}},
		{revision: "rev2", files: map[string]string{
			"compose.yaml": fmt.Sprintf(composeYAML, "1"), "app.env": "FOO=bar", "static/index.html": "static", "conf/app.conf": "v2",
		}},
		// Changes the config of the unchanged service, which recreates it with its unchanged files of the pinned artifact.
		{revision: "rev3", files: map[string]string{
			"compose.yaml": fmt.Sprintf(composeYAML, "2"), "app.env": "FOO=bar", "static/index.html": "static", "conf/app.conf": "v2",
		}},
	}

	deployConfig := &deploy.Config{Name: stackName, Timeout: duration.Duration(60 * time.Second)}
	payload := &webhook.ParsedPayload{CommitSHA: plumbing.ZeroHash, FullName: "kimdre/doco-cd_tests"}

	t.Cleanup(func() {
		service, err := compose.NewComposeService(dockerCli)
		if err != nil {
			t.Logf("failed to create compose service: %v", err)
			return
		}

		downCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		if err = service.Down(downCtx, stackName, api.DownOptions{RemoveOrphans: true, Volumes: true}); err != nil {
			t.Logf("failed to remove test stack: %v", err)
		}
	})

	deployRevision := func(revision string, files map[string]string) (string, map[string]container.Summary) {
		t.Helper()

		root := filepath.Join(storeBase, store.ArtifactsSubdir, revision)
		writePinningTestFiles(t, root, files)

		project, err := LoadCompose(ctx, nil, root, root, stackName, []string{filepath.Join(root, "compose.yaml")},
			nil, nil, map[string]string{}, ComposeLoadOptions{})
		if err != nil {
			t.Fatalf("failed to load compose project of %s: %v", revision, err)
		}

		addComposeServiceLabels(project, deployConfig, payload, "", root, "dev",
			time.Now().UTC().Format(time.RFC3339), ComposeVersion, revision, "hash-"+revision)

		err = deployCompose(ctx, dockerCli, project, deployConfig, api.RecreateDiverged, nil, nil, func(string) {}, nil,
			composeDeployOptions{ArtifactRoot: root})
		if err != nil {
			t.Fatalf("failed to deploy %s: %v", revision, err)
		}

		containers, err := composeServiceContainers(ctx, dockerCli.Client(), stackName)
		if err != nil {
			t.Fatal(err)
		}

		byService := make(map[string]container.Summary, len(containers))

		for service, serviceContainers := range containers {
			if len(serviceContainers) != 1 {
				t.Fatalf("service %s has %d containers after deploying %s, want 1", service, len(serviceContainers), revision)
			}

			byService[service] = serviceContainers[0]
		}

		return root, byService
	}

	mountSource := func(c container.Summary, target string) string {
		for _, m := range c.Mounts {
			if m.Destination == target {
				return m.Source
			}
		}

		return ""
	}

	root1, first := deployRevision(revisions[0].revision, revisions[0].files)
	root2, second := deployRevision(revisions[1].revision, revisions[1].files)

	if first["unchanged"].ID != second["unchanged"].ID {
		t.Errorf("service with unchanged files was recreated")
	}

	if first["generated"].ID != second["generated"].ID {
		t.Errorf("service with a bind mount created by docker was recreated")
	}

	if first["changed"].ID == second["changed"].ID {
		t.Errorf("service with changed files was not recreated")
	}

	if got, want := mountSource(second["changed"], "/conf"), filepath.Join(root2, "conf"); got != want {
		t.Errorf("changed service mounts %q, want %q", got, want)
	}

	_, third := deployRevision(revisions[2].revision, revisions[2].files)

	unchanged := third["unchanged"]
	if unchanged.ID == second["unchanged"].ID {
		t.Fatalf("service with a changed config was not recreated")
	}

	// Its bind-mounted files are still unchanged, so the recreated container keeps using them from the pinned artifact.
	if got, want := mountSource(unchanged, "/static"), filepath.Join(root1, "static"); got != want {
		t.Errorf("recreated service mounts %q, want the pinned artifact %q", got, want)
	}

	if got := unchanged.Labels[DocoCDLabels.Deployment.PinnedRevisions]; got != "rev1" {
		t.Errorf("recreated service pinned revisions label = %q, want %q", got, "rev1")
	}

	if got := unchanged.Labels[DocoCDLabels.Deployment.CommitSHA]; got != "rev3" {
		t.Errorf("recreated service commit label = %q, want %q", got, "rev3")
	}

	if third["changed"].ID != second["changed"].ID {
		t.Errorf("service with files unchanged since the previous revision was recreated")
	}
}
