package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"github.com/kimdre/doco-cd/internal/common/types/duration"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/source/store"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// liveTestProject returns a project loaded from artifactRoot with a service that ignores its
// ./conf bind mount and its app config, and a service that ignores nothing.
func liveTestProject(artifactRoot string) *types.Project {
	return &types.Project{
		Name: "stack",
		Services: types.Services{
			"proxy": {
				Name: "proxy",
				Labels: types.Labels{
					DocoCDLabels.Deployment.RecreateIgnore:       "{bindMounts: [/etc/proxy], configs: [app]}",
					DocoCDLabels.Deployment.RecreateIgnoreSignal: "SIGHUP",
				},
				Volumes: []types.ServiceVolumeConfig{
					{Type: types.VolumeTypeBind, Source: filepath.Join(artifactRoot, "conf"), Target: "/etc/proxy"},
					{Type: types.VolumeTypeBind, Source: filepath.Join(artifactRoot, "static"), Target: "/static"},
					{Type: types.VolumeTypeBind, Source: "/srv/certs", Target: "/etc/proxy-certs"},
				},
				Configs: []types.ServiceConfigObjConfig{{Source: "app"}, {Source: "other"}},
			},
			"web": {
				Name: "web",
				Volumes: []types.ServiceVolumeConfig{
					{Type: types.VolumeTypeBind, Source: filepath.Join(artifactRoot, "conf"), Target: "/etc/proxy"},
				},
				Configs: []types.ServiceConfigObjConfig{{Source: "other"}},
			},
		},
		Configs: types.Configs{
			"app":   {File: filepath.Join(artifactRoot, "app.conf")},
			"other": {File: filepath.Join(artifactRoot, "other.conf")},
		},
	}
}

func liveTestArtifact(t *testing.T, base, revision string, files map[string]string) string {
	t.Helper()

	root := filepath.Join(base, store.ArtifactsSubdir, revision)
	writePinningTestFiles(t, root, files)

	return root
}

func TestPrepareComposeLiveResources(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := liveTestArtifact(t, base, "rev1", map[string]string{"conf/proxy.conf": "v1", "app.conf": "app"})
	project := liveTestProject(root)

	live, err := prepareComposeLiveResources(project, root, "")
	if err != nil {
		t.Fatal(err)
	}

	wantDir := filepath.Join(base, store.LiveSubdir, DefaultContextName, "stack")
	if live.dir != wantDir {
		t.Fatalf("live dir = %q, want %q", live.dir, wantDir)
	}

	liveRoot := filepath.Join(wantDir, composeLiveRootDir)
	proxy := project.Services["proxy"]

	if got, want := proxy.Volumes[0].Source, filepath.Join(liveRoot, "conf"); got != want {
		t.Errorf("ignored bind mount source = %q, want %q", got, want)
	}

	if got, want := proxy.Volumes[1].Source, filepath.Join(root, "static"); got != want {
		t.Errorf("not ignored bind mount source = %q, want %q", got, want)
	}

	if got := proxy.Volumes[2].Source; got != "/srv/certs" {
		t.Errorf("bind mount outside the repository source = %q, want it unchanged", got)
	}

	if got, want := project.Configs["app"].File, filepath.Join(liveRoot, "app.conf"); got != want {
		t.Errorf("ignored config file = %q, want %q", got, want)
	}

	if got, want := project.Configs["other"].File, filepath.Join(root, "other.conf"); got != want {
		t.Errorf("not ignored config file = %q, want %q", got, want)
	}

	if got, want := project.Services["web"].Volumes[0].Source, filepath.Join(root, "conf"); got != want {
		t.Errorf("bind mount of a service without ignore rules = %q, want %q", got, want)
	}

	if got := proxy.Labels[DocoCDLabels.Deployment.LiveResources]; got != "app.conf,conf" {
		t.Errorf("live resources label = %q, want %q", got, "app.conf,conf")
	}

	if _, ok := project.Services["web"].Labels[DocoCDLabels.Deployment.LiveResources]; ok {
		t.Errorf("service without live resources has a live resources label")
	}

	if want := []string{"app.conf", "conf"}; !slices.Equal(live.resources, want) {
		t.Errorf("resources = %v, want %v", live.resources, want)
	}

	if got := live.signals["proxy"]; got != "SIGHUP" {
		t.Errorf("signal = %q, want SIGHUP", got)
	}

	// Preparing an already prepared project is a no-op.
	before := proxy.Volumes[0].Source

	if _, err = prepareComposeLiveResources(project, root, ""); err != nil {
		t.Fatal(err)
	}

	if got := project.Services["proxy"].Volumes[0].Source; got != before {
		t.Errorf("second preparation changed the source to %q, want %q", got, before)
	}
}

func TestPrepareComposeLiveResources_NotAnArtifact(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	live, err := prepareComposeLiveResources(liveTestProject(root), root, "")
	if err != nil || live != nil {
		t.Fatalf("prepareComposeLiveResources() = %v, %v, want nil, nil", live, err)
	}

	live, err = prepareComposeLiveResources(liveTestProject(root), "", "")
	if err != nil || live != nil {
		t.Fatalf("prepareComposeLiveResources() = %v, %v, want nil, nil", live, err)
	}
}

func readLiveTestFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(content)
}

func TestComposeLiveResourcesSync(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root1 := liveTestArtifact(t, base, "rev1", map[string]string{
		"conf/proxy.conf": "v1", "conf/removed.conf": "removed", "conf/sub/nested.conf": "nested", "app.conf": "app",
	})

	live, err := prepareComposeLiveResources(liveTestProject(root1), root1, "")
	if err != nil {
		t.Fatal(err)
	}

	changed, err := live.sync(true)
	if err != nil {
		t.Fatal(err)
	}

	wantChanged := []string{"app.conf", "conf", "conf/proxy.conf", "conf/removed.conf", "conf/sub", "conf/sub/nested.conf"}
	if !slices.Equal(changed, wantChanged) {
		t.Fatalf("changed = %v, want %v", changed, wantChanged)
	}

	proxyConf := filepath.Join(live.root, "conf", "proxy.conf")

	before, err := os.Stat(proxyConf)
	if err != nil {
		t.Fatal(err)
	}

	// A file written by the service itself.
	writePinningTestFiles(t, live.root, map[string]string{"conf/runtime.db": "data"})

	root2 := liveTestArtifact(t, base, "rev2", map[string]string{"conf/proxy.conf": "v2", "app.conf": "app"})

	live, err = prepareComposeLiveResources(liveTestProject(root2), root2, "")
	if err != nil {
		t.Fatal(err)
	}

	changed, err = live.sync(true)
	if err != nil {
		t.Fatal(err)
	}

	wantChanged = []string{"conf/proxy.conf", "conf/removed.conf", "conf/sub", "conf/sub/nested.conf"}
	if !slices.Equal(changed, wantChanged) {
		t.Fatalf("changed = %v, want %v", changed, wantChanged)
	}

	after, err := os.Stat(proxyConf)
	if err != nil {
		t.Fatal(err)
	}

	if !os.SameFile(before, after) {
		t.Error("updated live file was replaced instead of being updated in place")
	}

	if got := readLiveTestFile(t, proxyConf); got != "v2" {
		t.Errorf("live file content = %q, want v2", got)
	}

	if got := readLiveTestFile(t, filepath.Join(live.root, "conf", "runtime.db")); got != "data" {
		t.Errorf("file written by the service = %q, want it to be kept", got)
	}

	for _, removed := range []string{"conf/removed.conf", "conf/sub"} {
		if _, err := os.Lstat(filepath.Join(live.root, removed)); !os.IsNotExist(err) {
			t.Errorf("%s was not removed from the live copy: %v", removed, err)
		}
	}

	manifest, err := live.readManifest()
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"app.conf", "conf", "conf/proxy.conf"}; !slices.Equal(manifest.Entries, want) {
		t.Errorf("manifest entries = %v, want %v", manifest.Entries, want)
	}
}

func TestComposeLiveResourcesSync_SymlinkedResource(t *testing.T) {
	t.Parallel()

	base := t.TempDir()

	var live *composeLiveResources

	for i, content := range []string{"v1", "v2"} {
		root := liveTestArtifact(t, base, fmt.Sprintf("rev%d", i+1), map[string]string{"config/prod.conf": content})

		if err := os.Symlink("config/prod.conf", filepath.Join(root, "app.conf")); err != nil {
			t.Fatal(err)
		}

		var err error

		live, err = prepareComposeLiveResources(liveTestProject(root), root, "")
		if err != nil {
			t.Fatal(err)
		}

		changed, err := live.sync(true)
		if err != nil {
			t.Fatal(err)
		}

		if !slices.Contains(changed, "app.conf") {
			t.Errorf("revision %d: changed = %v, want it to contain app.conf", i+1, changed)
		}

		liveFile := filepath.Join(live.root, "app.conf")

		info, err := os.Lstat(liveFile)
		if err != nil {
			t.Fatal(err)
		}

		if !info.Mode().IsRegular() {
			t.Fatalf("revision %d: live copy of a symlinked resource has mode %v, want a regular file", i+1, info.Mode())
		}

		if got := readLiveTestFile(t, liveFile); got != content {
			t.Errorf("revision %d: live copy content = %q, want %q", i+1, got, content)
		}
	}
}

func TestComposeLiveResourcesSync_NotFull(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root1 := liveTestArtifact(t, base, "rev1", map[string]string{"conf/proxy.conf": "v1"})

	live, err := prepareComposeLiveResources(liveTestProject(root1), root1, "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err = live.sync(true); err != nil {
		t.Fatal(err)
	}

	root2 := liveTestArtifact(t, base, "rev2", map[string]string{"conf/proxy.conf": "v2", "app.conf": "app"})

	live, err = prepareComposeLiveResources(liveTestProject(root2), root2, "")
	if err != nil {
		t.Fatal(err)
	}

	changed, err := live.sync(false)
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"app.conf"}; !slices.Equal(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}

	if got := readLiveTestFile(t, filepath.Join(live.root, "conf", "proxy.conf")); got != "v1" {
		t.Errorf("existing live file = %q, want it to be left untouched", got)
	}

	manifest, err := live.readManifest()
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"app.conf", "conf", "conf/proxy.conf"}; !slices.Equal(manifest.Entries, want) {
		t.Errorf("manifest entries = %v, want %v", manifest.Entries, want)
	}
}

func TestComposeLiveResourcesPrune(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := liveTestArtifact(t, base, "rev1", map[string]string{"conf/proxy.conf": "v1", "app.conf": "app", "old/file": "x"})

	live, err := prepareComposeLiveResources(liveTestProject(root), root, "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err = live.sync(true); err != nil {
		t.Fatal(err)
	}

	// Simulate earlier live copies that are no longer used by the current resources.
	if _, _, err = filesystem.SyncInPlace(filepath.Join(root, "old"), filepath.Join(live.root, "old")); err != nil {
		t.Fatal(err)
	}

	if _, _, err = filesystem.SyncInPlace(filepath.Join(root, "old"), filepath.Join(live.root, "mounted")); err != nil {
		t.Fatal(err)
	}

	manifest, err := live.readManifest()
	if err != nil {
		t.Fatal(err)
	}

	if err = live.writeManifest(append(manifest.Entries, "old", "old/file", "mounted", "mounted/file")); err != nil {
		t.Fatal(err)
	}

	apiClient := &pinningTestClient{containers: []container.Summary{{
		Labels: map[string]string{api.ServiceLabel: "legacy"},
		Mounts: []container.MountPoint{{Type: mount.TypeBind, Source: filepath.Join(live.root, "mounted"), Destination: "/m"}},
	}}}

	if err = live.prune(context.Background(), apiClient, "stack"); err != nil {
		t.Fatal(err)
	}

	if _, err = os.Lstat(filepath.Join(live.root, "old")); !os.IsNotExist(err) {
		t.Errorf("unused live copy was not removed: %v", err)
	}

	for _, kept := range []string{"mounted/file", "conf/proxy.conf", "app.conf"} {
		if _, err = os.Lstat(filepath.Join(live.root, kept)); err != nil {
			t.Errorf("live copy %s in use was removed: %v", kept, err)
		}
	}

	manifest, err = live.readManifest()
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"app.conf", "conf", "conf/proxy.conf", "mounted", "mounted/file"}; !slices.Equal(manifest.Entries, want) {
		t.Errorf("manifest entries = %v, want %v", manifest.Entries, want)
	}

	// Without any resources and mounts, the whole live directory is removed.
	live.resources = nil

	if err = live.prune(context.Background(), &pinningTestClient{}, "stack"); err != nil {
		t.Fatal(err)
	}

	if _, err = os.Lstat(live.dir); !os.IsNotExist(err) {
		t.Errorf("empty live directory was not removed: %v", err)
	}

	if !filesystem.IsDir(filepath.Join(base, store.LiveSubdir)) {
		t.Errorf("shared live base directory was removed")
	}
}

func TestComposeLiveResourcesReadManifest_RejectsEscapingEntries(t *testing.T) {
	t.Parallel()

	live := &composeLiveResources{dir: t.TempDir()}
	live.root = filepath.Join(live.dir, composeLiveRootDir)

	content, err := json.Marshal(composeLiveManifest{Entries: []string{"ok", "../escape", "a/../../escape", "/abs", "a//b"}})
	if err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(filepath.Join(live.dir, composeLiveManifestFile), content, filesystem.PermPublic); err != nil {
		t.Fatal(err)
	}

	manifest, err := live.readManifest()
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"ok"}; !slices.Equal(manifest.Entries, want) {
		t.Errorf("manifest entries = %v, want %v", manifest.Entries, want)
	}
}

func TestComposeLiveResourcesSignalsFor(t *testing.T) {
	t.Parallel()

	live := &composeLiveResources{
		root:     "/data/repo/live/default/stack/root",
		services: map[string][]string{"proxy": {"conf"}, "silent": {"conf"}, "other": {"other.conf"}, "stopped": {"conf"}, "old": {"conf"}},
		signals:  map[string]string{"proxy": "SIGHUP", "other": "SIGUSR1", "stopped": "SIGHUP", "old": "SIGHUP"},
	}

	liveMount := []container.MountPoint{{Type: mount.TypeBind, Source: "/data/repo/live/default/stack/root/conf", Destination: "/conf"}}
	artifactMount := []container.MountPoint{{Type: mount.TypeBind, Source: "/data/repo/artifacts/rev1/conf", Destination: "/conf"}}

	apiClient := &pinningTestClient{containers: []container.Summary{
		{Labels: map[string]string{api.ServiceLabel: "proxy"}, State: container.StateRunning, Mounts: liveMount},
		{Labels: map[string]string{api.ServiceLabel: "silent"}, State: container.StateRunning, Mounts: liveMount},
		{Labels: map[string]string{api.ServiceLabel: "other"}, State: container.StateRunning, Mounts: liveMount},
		{Labels: map[string]string{api.ServiceLabel: "stopped"}, State: container.StateExited, Mounts: liveMount},
		{Labels: map[string]string{api.ServiceLabel: "old"}, State: container.StateRunning, Mounts: artifactMount},
	}}

	signals, err := live.signalsFor(context.Background(), apiClient, "stack", []string{"conf/proxy.conf"}, nil, api.RecreateDiverged, nil)
	if err != nil {
		t.Fatal(err)
	}

	if want := []SignalService{{ServiceName: "proxy", Signal: "SIGHUP"}}; !slices.Equal(signals, want) {
		t.Errorf("signals = %v, want %v", signals, want)
	}

	signals, err = live.signalsFor(context.Background(), apiClient, "stack", []string{"conf/proxy.conf"},
		[]SignalService{{ServiceName: "proxy", Signal: "SIGHUP"}}, api.RecreateDiverged, nil)
	if err != nil || len(signals) != 0 {
		t.Errorf("signals of already signaled service = %v, %v, want none", signals, err)
	}

	signals, err = live.signalsFor(context.Background(), apiClient, "stack", []string{"conf/proxy.conf"}, nil, api.RecreateForce, []string{"proxy"})
	if err != nil || len(signals) != 0 {
		t.Errorf("signals of force-recreated service = %v, %v, want none", signals, err)
	}

	signals, err = live.signalsFor(context.Background(), apiClient, "stack", []string{"conf/proxy.conf"}, nil, api.RecreateForce, nil)
	if err != nil || len(signals) != 0 {
		t.Errorf("signals when all services are force-recreated = %v, %v, want none", signals, err)
	}
}

func TestComposeLiveResourcesChangedServices(t *testing.T) {
	t.Parallel()

	live := &composeLiveResources{services: map[string][]string{
		"dir": {"conf"}, "file": {"conf/app.conf"}, "other": {"other"},
	}}

	testCases := []struct {
		changed []string
		want    []string
	}{
		{changed: []string{"conf/app.conf"}, want: []string{"dir", "file"}},
		{changed: []string{"conf/other.conf"}, want: []string{"dir"}},
		{changed: []string{"conf"}, want: []string{"dir", "file"}},
		{changed: []string{"configs"}, want: nil},
	}

	for _, tc := range testCases {
		if got := live.changedServices(tc.changed); !slices.Equal(got, tc.want) {
			t.Errorf("changedServices(%v) = %v, want %v", tc.changed, got, tc.want)
		}
	}
}

func TestComposeLiveDirsOfContainers(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	liveDir := filepath.Join(base, store.LiveSubdir, "remote", "stack")

	if err := os.MkdirAll(filepath.Join(base, store.ArtifactsSubdir), filesystem.PermDir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(liveDir, composeLiveRootDir, "conf"), filesystem.PermDir); err != nil {
		t.Fatal(err)
	}

	containers := map[string][]container.Summary{"proxy": {{Mounts: []container.MountPoint{
		{Type: mount.TypeBind, Source: filepath.Join(liveDir, composeLiveRootDir, "conf")},
		{Type: mount.TypeBind, Source: filepath.Join(liveDir, composeLiveRootDir, "conf")},
		{Type: mount.TypeBind, Source: filepath.Join(base, store.LiveSubdir, "remote", "other", composeLiveRootDir)},
		{Type: mount.TypeBind, Source: "/srv/live/remote/stack/root/x"},
		{Type: mount.TypeVolume, Source: filepath.Join(liveDir, composeLiveRootDir)},
	}}}}

	if got, want := composeLiveDirsOfContainers(containers, "stack"), []string{liveDir}; !slices.Equal(got, want) {
		t.Fatalf("composeLiveDirsOfContainers() = %v, want %v", got, want)
	}

	if err := removeComposeLiveDirs(containers, "stack"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(liveDir); !os.IsNotExist(err) {
		t.Fatalf("live directory was not removed: %v", err)
	}
}

func TestDeployComposeHotReloadsIgnoredResources(t *testing.T) {
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

	// On SIGHUP, the service records the content of its ignored files as it sees them.
	const composeYAML = `
services:
  proxy:
    image: alpine:3.21
    pull_policy: missing
    command: ["sh", "-c", "trap 'cat /conf/app.conf /single.conf /app-config > /tmp/reloaded' HUP; while true; do sleep 0.2 & wait $$!; done"]
    stop_grace_period: 1s
    labels:
%s
    volumes:
      - ./conf:/conf:ro
      - ./single.conf:/single.conf:ro
    configs:
      - source: app
        target: /app-config
configs:
  app:
    file: ./app.conf
`

	const ignoreLabels = `      cd.doco.deployment.recreate.ignore: "{bindMounts: [/conf, /single.conf], configs: [app]}"
      cd.doco.deployment.recreate.ignore.signal: SIGHUP`

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

	deployRevision := func(revision, labels, content string) container.Summary {
		t.Helper()

		root := liveTestArtifact(t, storeBase, revision, map[string]string{
			"compose.yaml":  fmt.Sprintf(composeYAML, labels),
			"conf/app.conf": "dir-" + content + "\n",
			"single.conf":   "file-" + content + "\n",
			"app.conf":      "config-" + content + "\n",
		})

		project, err := LoadCompose(ctx, nil, root, root, stackName, []string{filepath.Join(root, "compose.yaml")},
			nil, nil, map[string]string{}, ComposeLoadOptions{})
		if err != nil {
			t.Fatalf("failed to load compose project of %s: %v", revision, err)
		}

		addComposeServiceLabels(project, deployConfig, payload, "", root, "dev",
			time.Now().UTC().Format(time.RFC3339), ComposeVersion, revision, "hash-"+revision)

		err = deployCompose(ctx, dockerCli, project, deployConfig, api.RecreateDiverged, nil, nil, func(string) {}, nil,
			composeDeployOptions{ArtifactRoot: root, SyncLive: true})
		if err != nil {
			t.Fatalf("failed to deploy %s: %v", revision, err)
		}

		containers, err := composeServiceContainers(ctx, dockerCli.Client(), stackName)
		if err != nil {
			t.Fatal(err)
		}

		if len(containers["proxy"]) != 1 {
			t.Fatalf("service has %d containers after deploying %s, want 1", len(containers["proxy"]), revision)
		}

		return containers["proxy"][0]
	}

	mountSource := func(c container.Summary, target string) string {
		for _, m := range c.Mounts {
			if m.Destination == target {
				return m.Source
			}
		}

		return ""
	}

	liveRoot := filepath.Join(storeBase, store.LiveSubdir, DefaultContextName, stackName, composeLiveRootDir)
	wantContent := func(content string) string {
		return "dir-" + content + "\nfile-" + content + "\nconfig-" + content + "\n"
	}

	first := deployRevision("rev1", ignoreLabels, "v1")

	for target, rel := range map[string]string{"/conf": "conf", "/single.conf": "single.conf", "/app-config": "app.conf"} {
		if got, want := mountSource(first, target), filepath.Join(liveRoot, rel); got != want {
			t.Errorf("%s is mounted from %q, want the live copy %q", target, got, want)
		}
	}

	second := deployRevision("rev2", ignoreLabels, "v2")

	if second.ID != first.ID {
		t.Fatalf("service with only ignored changes was recreated")
	}

	got, err := Exec(dockerCli.Client(), second.ID, "cat", "/conf/app.conf", "/single.conf", "/app-config")
	if err != nil {
		t.Fatal(err)
	}

	if want := wantContent("v2"); got != want {
		t.Errorf("service sees %q, want the updated content %q", got, want)
	}

	// The signal is sent after the live copies were updated.
	deadline := time.Now().Add(10 * time.Second)

	for {
		got, err = Exec(dockerCli.Client(), second.ID, "cat", "/tmp/reloaded")
		if err == nil && got == wantContent("v2") {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("service saw %q on reload (err: %v), want %q", got, err, wantContent("v2"))
		}

		time.Sleep(200 * time.Millisecond)
	}

	// Without the ignore rules, the service is recreated from the artifact and the live copies are removed.
	third := deployRevision("rev3", "      app: test", "v3")

	if third.ID == second.ID {
		t.Fatalf("service was not recreated after removing its ignore rules")
	}

	if got, want := mountSource(third, "/conf"), filepath.Join(storeBase, store.ArtifactsSubdir, "rev3", "conf"); got != want {
		t.Errorf("/conf is mounted from %q, want %q", got, want)
	}

	if _, err = os.Lstat(filepath.Dir(liveRoot)); !os.IsNotExist(err) {
		t.Errorf("unused live directory was not removed: %v", err)
	}
}
