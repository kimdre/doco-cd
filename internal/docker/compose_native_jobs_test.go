package docker

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/types/duration"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// recordingCompose stores the projects that reach the wrapped Compose service.
type recordingCompose struct {
	api.Compose

	created []*types.Project
	started []*types.Project
	upped   []*types.Project
}

func (r *recordingCompose) Create(_ context.Context, project *types.Project, _ api.CreateOptions) error {
	r.created = append(r.created, project)

	return nil
}

func (r *recordingCompose) Up(_ context.Context, project *types.Project, _ api.UpOptions) error {
	r.upped = append(r.upped, project)

	return nil
}

func (r *recordingCompose) Start(_ context.Context, _ string, options api.StartOptions) error {
	r.started = append(r.started, options.Project)

	return nil
}

func projectWithJobs() *types.Project {
	return &types.Project{
		Name:         "test",
		Services:     types.Services{"app": {Name: "app"}},
		Jobs:         types.Jobs{"backup": {Name: "backup"}},
		DisabledJobs: types.Jobs{"report": {Name: "report"}},
	}
}

// assertNativeJobsDisabled checks that Compose sees no active jobs but can still find all declared jobs.
// Compose detects orphan jobs through AllJobs, so the declared names must stay in DisabledJobs.
func assertNativeJobsDisabled(t *testing.T, project *types.Project) {
	t.Helper()

	if project == nil {
		t.Fatal("project is nil")
	}

	if len(project.Jobs) != 0 {
		t.Fatalf("project has active jobs: %v", project.Jobs)
	}

	if got := slices.Sorted(maps.Keys(project.AllJobs())); !slices.Equal(got, []string{"backup", "report"}) {
		t.Fatalf("declared jobs = %v, want [backup report]", got)
	}
}

func assertOriginalJobs(t *testing.T, project *types.Project) {
	t.Helper()

	if _, ok := project.Jobs["backup"]; !ok || len(project.Jobs) != 1 {
		t.Fatalf("original project jobs = %v, want [backup]", project.Jobs)
	}

	if _, ok := project.DisabledJobs["report"]; !ok || len(project.DisabledJobs) != 1 {
		t.Fatalf("original project disabled jobs = %v, want [report]", project.DisabledJobs)
	}
}

func TestWithoutNativeJobs(t *testing.T) {
	t.Parallel()

	if got := withoutNativeJobs(nil); got != nil {
		t.Fatalf("withoutNativeJobs(nil) = %v, want nil", got)
	}

	plain := &types.Project{Name: "plain", Services: types.Services{"app": {Name: "app"}}}
	if got := withoutNativeJobs(plain); got != plain {
		t.Fatal("a project without jobs must be returned as is")
	}

	original := projectWithJobs()

	stripped := withoutNativeJobs(original)
	if stripped == original {
		t.Fatal("a project with jobs must be copied")
	}

	assertNativeJobsDisabled(t, stripped)

	if _, ok := stripped.Services["app"]; !ok || stripped.Name != "test" {
		t.Fatalf("stripped project lost its services or name: %#v", stripped)
	}

	// The copy must not share the job maps with the original project.
	delete(stripped.DisabledJobs, "backup")
	assertOriginalJobs(t, original)
}

func TestComposeServiceWithoutNativeJobs(t *testing.T) {
	t.Parallel()

	recorder := &recordingCompose{}
	service := composeServiceWithoutNativeJobs{Compose: recorder}
	original := projectWithJobs()

	if err := service.Create(t.Context(), original, api.CreateOptions{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := service.Up(t.Context(), original, api.UpOptions{}); err != nil {
		t.Fatalf("Up() error = %v", err)
	}

	if err := service.Start(t.Context(), "test", api.StartOptions{Project: original}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if err := service.Start(t.Context(), "test", api.StartOptions{}); err != nil {
		t.Fatalf("Start() without project error = %v", err)
	}

	if len(recorder.started) != 2 {
		t.Fatalf("Start() reached Compose %d times, want 2", len(recorder.started))
	}

	for name, projects := range map[string][]*types.Project{"Create": recorder.created, "Up": recorder.upped, "Start": recorder.started[:1]} {
		if len(projects) != 1 {
			t.Fatalf("%s() reached Compose %d times, want 1", name, len(projects))
		}

		if projects[0] == original {
			t.Fatalf("%s() passed the original project to Compose", name)
		}

		assertNativeJobsDisabled(t, projects[0])
	}

	if recorder.started[1] != nil {
		t.Fatalf("Start() without project passed %v, want nil", recorder.started[1])
	}

	assertOriginalJobs(t, original)
}

func TestWarnSkippedNativeJobs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, nil))

	warnSkippedNativeJobs(logger, &types.Project{Name: "plain"})
	warnSkippedNativeJobs(nil, projectWithJobs())

	if buf.Len() != 0 {
		t.Fatalf("unexpected log output: %s", buf.String())
	}

	warnSkippedNativeJobs(logger, projectWithJobs())

	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "backup") || !strings.Contains(out, "report") {
		t.Fatalf("log output %q misses the warning or job names", out)
	}
}

func TestWarnSwarmNativeJobs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, nil))

	warnSwarmNativeJobs(logger, &types.Project{Name: "plain"})
	warnSwarmNativeJobs(nil, projectWithJobs())

	if buf.Len() != 0 {
		t.Fatalf("unexpected log output: %s", buf.String())
	}

	warnSwarmNativeJobs(logger, projectWithJobs())

	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "backup") {
		t.Fatalf("log output %q misses the warning or job names", out)
	}
}

// TestDeployComposeWithScheduledNativeJob checks that a scheduled native job does not reach the Docker Engine.
// Compose 5.8.0 rejects scheduled jobs on Engines older than 29.10 and registers them on newer Engines.
// On older Engines, the deploy fails without the guard. The Compose jobs registry API is internal to Compose,
// so this test cannot read the registry. TestComposeServiceWithoutNativeJobs checks that Compose gets no active jobs.
func TestDeployComposeWithScheduledNativeJob(t *testing.T) {
	ctx := context.Background()

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
	root := t.TempDir()
	composePath := filepath.Join(root, "compose.yaml")

	const composeYAML = `
services:
  app:
    image: alpine:3.22
    pull_policy: missing
    command: ["sleep", "infinity"]
    stop_grace_period: 1s
jobs:
  backup:
    image: alpine:3.22
    pull_policy: missing
    command: ["true"]
    triggers:
      schedule:
        - cron: "0 3 * * *"
`

	if err = os.WriteFile(composePath, []byte(composeYAML), filesystem.PermPublic); err != nil {
		t.Fatal(err)
	}

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

	project, err := LoadCompose(ctx, nil, root, root, stackName, []string{composePath}, nil, nil, map[string]string{}, ComposeLoadOptions{})
	if err != nil {
		t.Fatalf("failed to load compose project: %v", err)
	}

	if len(project.AllJobs()) != 1 {
		t.Fatalf("loaded project has %d native jobs, want 1", len(project.AllJobs()))
	}

	// remove_orphans is on by default. It must not remove the declared native job.
	deployConfig := &deploy.Config{Name: stackName, Timeout: duration.Duration(60 * time.Second), RemoveOrphans: true}
	payload := &webhook.ParsedPayload{CommitSHA: plumbing.ZeroHash, FullName: "kimdre/doco-cd_tests"}

	addComposeServiceLabels(project, deployConfig, payload, "", root, "dev",
		time.Now().UTC().Format(time.RFC3339), ComposeVersion, "rev1", "hash-rev1")

	err = deployCompose(ctx, dockerCli, project, deployConfig, api.RecreateDiverged, nil, nil, func(string) {}, nil,
		composeDeployOptions{ArtifactRoot: root})
	if err != nil {
		t.Fatalf("failed to deploy project with a scheduled native job: %v", err)
	}

	containers, err := composeServiceContainers(ctx, dockerCli.Client(), stackName)
	if err != nil {
		t.Fatal(err)
	}

	if len(containers) != 1 || len(containers["app"]) != 1 {
		t.Fatalf("got containers %v, want one container of service app", containers)
	}

	// composeServiceContainers skips job runs, so check the unfiltered project containers too.
	all, err := dockerCli.Client().ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", api.ProjectLabel+"="+stackName),
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range all.Items {
		if isEngineJobRun(c.Labels) {
			t.Fatalf("container %s is a native job run", c.ID)
		}
	}

	if len(project.AllJobs()) != 1 {
		t.Fatal("deploy removed the native job from the loaded project")
	}
}
