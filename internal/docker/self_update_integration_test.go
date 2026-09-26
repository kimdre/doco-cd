package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	composeCli "github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/selfupdate"
	internaltest "github.com/kimdre/doco-cd/internal/test"
)

const selfUpdateIntegrationEnvVar = "DOCO_CD_RUN_DOCKER_INTEGRATION_TESTS"

func requireSelfUpdateIntegrationGate(t *testing.T) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping self-update Docker integration tests in short mode")
	}

	if os.Getenv(selfUpdateIntegrationEnvVar) != "1" {
		t.Skipf("set %s=1 to run self-update Docker integration tests", selfUpdateIntegrationEnvVar)
	}
}

// loadSelfUpdateProject loads a project from inline YAML with the CustomLabels
// compose needs to track containers, mirroring internaltest.ComposeUp.
func loadSelfUpdateProject(ctx context.Context, t *testing.T, stackName, yaml string) *types.Project {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")

	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	opts, err := composeCli.NewProjectOptions(
		[]string{path},
		composeCli.WithWorkingDirectory(dir),
		composeCli.WithName(stackName),
	)
	if err != nil {
		t.Fatalf("project options: %v", err)
	}

	project, err := opts.LoadProject(ctx)
	if err != nil {
		t.Fatalf("load project: %v", err)
	}

	for i, s := range project.Services {
		s.CustomLabels = map[string]string{
			api.ProjectLabel:     project.Name,
			api.ServiceLabel:     s.Name,
			api.WorkingDirLabel:  project.WorkingDir,
			api.ConfigFilesLabel: strings.Join(project.ComposeFiles, ","),
			api.VersionLabel:     api.ComposeVersion,
			api.OneoffLabel:      "False",
		}
		project.Services[i] = s
	}

	return project
}

func selfUpdateStackContainers(ctx context.Context, t *testing.T, cli client.APIClient, stack, service string) []container.Summary {
	t.Helper()

	f := make(client.Filters).
		Add("label", api.ProjectLabel+"="+stack).
		Add("label", api.ServiceLabel+"="+service)

	list, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}

	return list.Items
}

const selfUpdateSleepYAML = `
services:
  app:
    image: alpine:3.22
    command: ["sleep", "600"]
    restart: %s
    environment:
      GEN: "%s"
`

// TestSelfUpdateIntegration_ScaleOutLeavesDivergedRunning answers open question 1:
// does RecreateNever + scale 2 leave the diverged container #1 running and create
// a correctly labeled #2? Strategy C depends entirely on this.
func TestSelfUpdateIntegration_ScaleOutLeavesDivergedRunning(t *testing.T) {
	requireSelfUpdateIntegrationGate(t)

	ctx := t.Context()
	stackName := internaltest.ConvertTestName(t.Name())

	stack := internaltest.ComposeUp(ctx, t,
		internaltest.WithName(stackName),
		internaltest.WithYAML(fmt.Sprintf(selfUpdateSleepYAML, "unless-stopped", "1")),
	)

	id1 := stack.ServiceContainerID(ctx, t, "app")

	insp1, err := stack.Client.ContainerInspect(ctx, id1, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect #1: %v", err)
	}

	hash1 := insp1.Container.Config.Labels[api.ConfigHashLabel]
	if hash1 == "" {
		t.Fatal("container #1 has no config-hash label")
	}

	project2 := loadSelfUpdateProject(ctx, t, stackName, fmt.Sprintf(selfUpdateSleepYAML, "unless-stopped", "2"))

	svc2 := project2.Services["app"]

	wantHash, err := compose.ServiceHash(svc2)
	if err != nil {
		t.Fatalf("service hash: %v", err)
	}

	if wantHash == hash1 {
		t.Fatal("GEN change did not alter the service hash, test is not exercising divergence")
	}

	two := 2
	svc2.Scale = &two
	project2.Services["app"] = svc2

	hashAfterScale, err := compose.ServiceHash(svc2)
	if err != nil {
		t.Fatalf("service hash after scale: %v", err)
	}

	if hashAfterScale != wantHash {
		t.Errorf("scale changed the service hash: %s != %s", hashAfterScale, wantHash)
	}

	err = stack.Service.Create(ctx, project2, api.CreateOptions{
		Services:             []string{"app"},
		Recreate:             api.RecreateNever,
		RecreateDependencies: api.RecreateNever,
		IgnoreOrphans:        true,
		QuietPull:            true,
	})
	if err != nil {
		t.Fatalf("scale-out create failed: %v", err)
	}

	insp1After, err := stack.Client.ContainerInspect(ctx, id1, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect #1 after create: %v", err)
	}

	if !insp1After.Container.State.Running {
		t.Errorf("container #1 is not running after scale-out: status=%s", insp1After.Container.State.Status)
	}

	list := selfUpdateStackContainers(ctx, t, stack.Client, stackName, "app")
	if len(list) != 2 {
		for _, c := range list {
			t.Logf("container %s names=%v state=%s number=%s", c.ID[:12], c.Names, string(c.State), c.Labels[api.ContainerNumberLabel])
		}

		t.Fatalf("want 2 containers after scale-out, got %d", len(list))
	}

	var newID string

	for _, c := range list {
		if c.ID != id1 {
			newID = c.ID
		}
	}

	insp2, err := stack.Client.ContainerInspect(ctx, newID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect #2: %v", err)
	}

	labels := insp2.Container.Config.Labels

	if got := labels[api.ContainerNumberLabel]; got != "2" {
		t.Errorf("container #2 number label = %q, want %q", got, "2")
	}

	if got := labels[api.ConfigHashLabel]; got != wantHash {
		t.Errorf("container #2 config-hash = %q, want %q", got, wantHash)
	}

	if got := insp2.Container.State.Status; got != "created" {
		t.Logf("container #2 state after Create = %q (expected \"created\")", got)
	}

	if _, err = stack.Client.ContainerStart(ctx, newID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start #2: %v", err)
	}

	insp2, err = stack.Client.ContainerInspect(ctx, newID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect #2 after start: %v", err)
	}

	if !insp2.Container.State.Running {
		t.Errorf("container #2 not running after start: %s", insp2.Container.State.Status)
	}

	insp1After, err = stack.Client.ContainerInspect(ctx, id1, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("re-inspect #1: %v", err)
	}

	if !insp1After.Container.State.Running {
		t.Errorf("container #1 stopped after #2 started: %s", insp1After.Container.State.Status)
	}

	t.Logf("Q1 RESULT: #1 %s running=%v, #2 %s running=%v number=%s",
		id1[:12], insp1After.Container.State.Running,
		newID[:12], insp2.Container.State.Running, labels[api.ContainerNumberLabel])

	// Scale back to 1: compose must keep the new container and drop the diverged one.
	one := 1
	svc2.Scale = &one
	project2.Services["app"] = svc2

	err = stack.Service.Create(ctx, project2, api.CreateOptions{
		Services:             []string{"app"},
		Recreate:             api.RecreateDiverged,
		RecreateDependencies: api.RecreateNever,
		IgnoreOrphans:        true,
		QuietPull:            true,
	})
	if err != nil {
		t.Fatalf("scale-back create failed: %v", err)
	}

	list = selfUpdateStackContainers(ctx, t, stack.Client, stackName, "app")

	var survivors []string
	for _, c := range list {
		survivors = append(survivors, fmt.Sprintf("%s/%s/%s", c.ID[:12], c.Labels[api.ContainerNumberLabel], c.State))
	}

	t.Logf("Q1 SCALE-BACK RESULT: %d containers: %v (id1=%s new=%s)", len(list), survivors, id1[:12], newID[:12])
}

// TestSelfUpdateIntegration_RestartPolicyDormantAfterAPIStop answers open question 2:
// Docker must not restart a container that was stopped through the API, otherwise
// the successor cannot remove the predecessor without it coming back.
func TestSelfUpdateIntegration_RestartPolicyDormantAfterAPIStop(t *testing.T) {
	requireSelfUpdateIntegrationGate(t)

	for _, policy := range []string{"unless-stopped", "always"} {
		t.Run(policy, func(t *testing.T) {
			ctx := t.Context()
			stackName := internaltest.ConvertTestName(t.Name())

			stack := internaltest.ComposeUp(ctx, t,
				internaltest.WithName(stackName),
				internaltest.WithYAML(fmt.Sprintf(selfUpdateSleepYAML, policy, "1")),
			)

			id := stack.ServiceContainerID(ctx, t, "app")

			timeout := 1
			if _, err := stack.Client.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
				t.Fatalf("stop: %v", err)
			}

			time.Sleep(5 * time.Second)

			insp, err := stack.Client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
			if err != nil {
				t.Fatalf("inspect after stop: %v", err)
			}

			t.Logf("Q2 RESULT (%s): running=%v status=%s restartCount=%d",
				policy, insp.Container.State.Running, insp.Container.State.Status, insp.Container.RestartCount)

			if insp.Container.State.Running {
				t.Errorf("container restarted after API stop with policy %q", policy)
			}

			if insp.Container.RestartCount != 0 {
				t.Errorf("restart count = %d, want 0", insp.Container.RestartCount)
			}

			// Control: the policy must be live for a container that exits on its
			// own. An API stop or kill both count as a manual stop, and PID 1
			// cannot SIGKILL itself from inside its own namespace, so neither is
			// usable as a control here.
			controlName := internaltest.ConvertTestName(t.Name()) + "-control"

			controlStack := internaltest.ComposeUp(ctx, t,
				internaltest.WithName(controlName),
				internaltest.WithNoWait(),
				internaltest.WithYAML(fmt.Sprintf(`
services:
  app:
    image: alpine:3.22
    command: ["sh", "-c", "sleep 3; exit 7"]
    restart: %s
`, policy)),
			)

			controlID := controlStack.ServiceContainerID(ctx, t, "app")

			deadline := time.Now().Add(30 * time.Second)
			revived := false

			for time.Now().Before(deadline) {
				insp, err = controlStack.Client.ContainerInspect(ctx, controlID, client.ContainerInspectOptions{})
				if err != nil {
					t.Fatalf("inspect during control case: %v", err)
				}

				if insp.Container.RestartCount > 0 {
					revived = true
					break
				}

				time.Sleep(500 * time.Millisecond)
			}

			t.Logf("Q2 CONTROL (%s): self-exiting container restarted=%v restartCount=%d", policy, revived, insp.Container.RestartCount)

			if !revived {
				t.Errorf("self-exiting container did not restart, policy %q is not live and the result above is meaningless", policy)
			}
		})
	}
}

// TestSelfUpdateIntegration_DisabledServiceIsNotOrphan proves the self container
// survives a deploy of the rest of the project with RemoveOrphans on.
func TestSelfUpdateIntegration_DisabledServiceIsNotOrphan(t *testing.T) {
	requireSelfUpdateIntegrationGate(t)

	ctx := t.Context()
	stackName := internaltest.ConvertTestName(t.Name())

	const yaml = `
services:
  app:
    image: alpine:3.22
    command: ["sleep", "600"]
  sidecar:
    image: alpine:3.22
    command: ["sleep", "600"]
`

	stack := internaltest.ComposeUp(ctx, t,
		internaltest.WithName(stackName),
		internaltest.WithYAML(yaml),
	)

	appID := stack.ServiceContainerID(ctx, t, "app")

	project := loadSelfUpdateProject(ctx, t, stackName, yaml)

	disabled := project.WithServicesDisabled("app")

	err := stack.Service.Create(ctx, disabled, api.CreateOptions{
		RemoveOrphans:        true,
		Recreate:             api.RecreateDiverged,
		RecreateDependencies: api.RecreateNever,
		QuietPull:            true,
	})
	if err != nil {
		t.Fatalf("create with disabled service: %v", err)
	}

	insp, err := stack.Client.ContainerInspect(ctx, appID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("app container gone after RemoveOrphans deploy: %v", err)
	}

	t.Logf("DISABLED-ORPHAN RESULT: app running=%v", insp.Container.State.Running)

	if !insp.Container.State.Running {
		t.Errorf("app container not running after deploying the rest of the project")
	}
}

// TestSelfUpdateIntegration_NetworkHashMatchesLiveLabel pins the drift rule the
// strategy selector uses to detect a network recreate.
func TestSelfUpdateIntegration_NetworkHashMatchesLiveLabel(t *testing.T) {
	requireSelfUpdateIntegrationGate(t)

	ctx := t.Context()
	stackName := internaltest.ConvertTestName(t.Name())

	yamlFor := func(label string) string {
		return fmt.Sprintf(`
services:
  app:
    image: alpine:3.22
    command: ["sleep", "600"]
    networks:
      - backend
networks:
  backend:
    labels:
      x: "%s"
`, label)
	}

	stack := internaltest.ComposeUp(ctx, t,
		internaltest.WithName(stackName),
		internaltest.WithYAML(yamlFor("1")),
	)

	project := loadSelfUpdateProject(ctx, t, stackName, yamlFor("1"))

	netCfg := project.Networks["backend"]

	wantHash, err := compose.NetworkHash(&netCfg)
	if err != nil {
		t.Fatalf("network hash: %v", err)
	}

	nets, err := stack.Client.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("label", api.ProjectLabel+"="+stackName),
	})
	if err != nil {
		t.Fatalf("list networks: %v", err)
	}

	if len(nets.Items) != 1 {
		t.Fatalf("want 1 project network, got %d", len(nets.Items))
	}

	liveHash := nets.Items[0].Labels[api.ConfigHashLabel]

	t.Logf("NETWORK HASH RESULT: live=%q computed=%q match=%v", liveHash, wantHash, liveHash == wantHash)

	if liveHash != wantHash {
		t.Errorf("computed network hash does not match the live label")
	}

	project2 := loadSelfUpdateProject(ctx, t, stackName, yamlFor("2"))
	netCfg2 := project2.Networks["backend"]

	hash2, err := compose.NetworkHash(&netCfg2)
	if err != nil {
		t.Fatalf("network hash 2: %v", err)
	}

	if hash2 == wantHash {
		t.Errorf("changing a network label did not change the hash")
	}
}

// TestSelfUpdateIntegration_StartWithExitedAndCreatedTemp answers open question 5:
// what compose Start does when a service has one exited container and one created
// container under a temp name, which is the state a crashed applier leaves behind.
func TestSelfUpdateIntegration_StartWithExitedAndCreatedTemp(t *testing.T) {
	requireSelfUpdateIntegrationGate(t)

	ctx := t.Context()
	stackName := internaltest.ConvertTestName(t.Name())

	yaml := fmt.Sprintf(selfUpdateSleepYAML, "no", "1")

	stack := internaltest.ComposeUp(ctx, t,
		internaltest.WithName(stackName),
		internaltest.WithYAML(yaml),
	)

	id1 := stack.ServiceContainerID(ctx, t, "app")

	insp, err := stack.Client.ContainerInspect(ctx, id1, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect #1: %v", err)
	}

	timeout := 1
	if _, err = stack.Client.ContainerStop(ctx, id1, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		t.Fatalf("stop #1: %v", err)
	}

	cfg := *insp.Container.Config
	cfg.Labels = maps.Clone(insp.Container.Config.Labels)
	hostCfg := *insp.Container.HostConfig

	tmpName := id1[:12] + "_" + strings.TrimPrefix(insp.Container.Name, "/")

	created, err := stack.Client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &cfg,
		HostConfig: &hostCfg,
		Name:       tmpName,
	})
	if err != nil {
		t.Fatalf("create temp-named clone: %v", err)
	}

	project := loadSelfUpdateProject(ctx, t, stackName, yaml)

	err = stack.Service.Start(ctx, stackName, api.StartOptions{Project: project})
	if err != nil {
		t.Logf("Q5: compose Start returned error: %v", err)
	}

	list := selfUpdateStackContainers(ctx, t, stack.Client, stackName, "app")

	running := make([]string, 0, len(list))

	for _, c := range list {
		label := c.ID[:12]
		if c.ID == id1 {
			label += "(original)"
		}

		if c.ID == created.ID {
			label += "(temp-named)"
		}

		running = append(running, label+"="+string(c.State))
	}

	t.Logf("Q5 RESULT: %d containers after Start: %v", len(list), running)
}

// selfNetworkDriftYAML builds a self stack with controllable network and
// health settings for Docker integration tests.
func selfNetworkDriftYAML(label, generation, health string) string {
	return fmt.Sprintf(`
services:
  app:
    image: alpine:3.22
    command: ["sleep", "600"]
    restart: unless-stopped
    environment:
      GENERATION: "%s"
    healthcheck:
      test: ["CMD", "%s"]
      interval: 1s
      retries: 2
    networks: [backend]
  sidecar:
    image: alpine:3.22
    command: ["sleep", "600"]
    environment:
      GENERATION: "%s"
    networks: [backend]
networks:
  backend:
    labels:
      generation: "%s"
`, generation, health, generation, label)
}

type failingRollbackStartClient struct {
	client.APIClient
}

// ContainerStart injects a failure while restarting a rollback container.
func (c failingRollbackStartClient) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	return client.ContainerStartResult{}, errors.New("cannot start previous container yet")
}

// TestSelfUpdateIntegration_NetworkDrift exercises recreation and rollback
// of project networks under the applier strategy.
func TestSelfUpdateIntegration_NetworkDrift(t *testing.T) {
	requireSelfUpdateIntegrationGate(t)

	for _, mode := range []string{"healthy", "unhealthy", "crash", "crash-before-detach", "rollback-retry"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			stackName := internaltest.ConvertTestName(t.Name())
			oldYAML := selfNetworkDriftYAML("old", "old", "true")
			stack := internaltest.ComposeUp(ctx, t,
				internaltest.WithName(stackName),
				internaltest.WithYAML(oldYAML),
			)
			originalApp := stack.ServiceContainerID(ctx, t, "app")
			originalSidecar := stack.ServiceContainerID(ctx, t, "sidecar")

			health := "true"
			if mode == "unhealthy" {
				health = "false"
			}

			project := loadSelfUpdateProject(ctx, t, stackName, selfNetworkDriftYAML("new", "new", health))

			drift, err := networkDrift(ctx, stack.Client, project, project.Services["app"])
			if err != nil || !drift {
				t.Fatalf("networkDrift() = %v, %v; want true", drift, err)
			}

			snapshot, err := captureSelfDriftSnapshot(ctx, stack.Client, project)
			if err != nil {
				t.Fatal(err)
			}

			store := selfupdate.NewStore(t.TempDir())

			record := selfupdate.Record{
				ID: "drift", State: selfupdate.StateApplying, Strategy: selfupdate.StrategyApplier,
				Stack: stackName, Service: "app",
				Predecessor: selfupdate.ContainerRef{ID: originalApp, Number: 1},
				Drift:       snapshot,
				Deploy:      selfupdate.DeployInfo{NetworkDrift: true, RecreateMode: api.RecreateDiverged, TimeoutSeconds: 12},
			}
			if err = store.Create(&record); err != nil {
				t.Fatal(err)
			}

			// Model the throwaway applier independently of doco-cd's binary. It
			// must survive removal of the project's network.
			original, err := stack.Client.ContainerInspect(ctx, originalApp, client.ContainerInspectOptions{})
			if err != nil {
				t.Fatal(err)
			}

			applierOpts := BuildSelfApplierCreate(original.Container, record.ID, stackName, true)
			applierOpts.Config.Cmd = []string{"sleep", "600"}

			clone, err := stack.Client.ContainerCreate(ctx, applierOpts)
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() {
				_, _ = stack.Client.ContainerRemove(context.WithoutCancel(ctx), clone.ID, client.ContainerRemoveOptions{Force: true})
			})

			record.Applier = selfupdate.ContainerRef{ID: clone.ID}
			if err = store.Save(record); err != nil {
				t.Fatal(err)
			}

			if err = connectSelfApplierNetworks(ctx, stack.Client, clone.ID, original.Container, nil); err != nil {
				t.Fatalf("join predecessor network for preflight: %v", err)
			}

			if _, err = stack.Client.ContainerStart(ctx, clone.ID, client.ContainerStartOptions{}); err != nil {
				t.Fatal(err)
			}

			if mode != "crash-before-detach" {
				if err = detachSelfApplierProjectNetworks(ctx, stack.Client, record); err != nil {
					t.Fatalf("detach clone before network update: %v", err)
				}
			}

			if mode == "crash" || mode == "crash-before-detach" || mode == "rollback-retry" {
				record.DriftStarted = true
				if mode == "rollback-retry" {
					record.Error = "initialize secret provider: credentials unavailable"
				}

				if err = store.Save(record); err != nil {
					t.Fatal(err)
				}

				if mode != "crash-before-detach" {
					err = stack.Service.Create(ctx, project, api.CreateOptions{
						Recreate: api.RecreateDiverged, RecreateDependencies: api.RecreateDiverged, QuietPull: true,
					})
					if err != nil {
						t.Fatalf("partially apply network update: %v", err)
					}
				}

				if mode == "rollback-retry" {
					cli := selfApplyTestCli{apiClient: failingRollbackStartClient{APIClient: stack.Client}}
					if err = ApplySelfUpdate(ctx, cli, ApplySelfOptions{Store: store, JournalID: record.ID, Log: slog.Default()}); err == nil {
						t.Fatal("incomplete network rollback returned success")
					}

					pending, loadErr := store.Load(record.ID)
					if loadErr != nil || pending.State != selfupdate.StateApplying ||
						!strings.Contains(pending.Error, "cannot start previous container yet") ||
						!strings.Contains(pending.Error, record.Error) {
						t.Fatalf("incomplete rollback journal = %s/%q (%v); want applying with cause",
							pending.State, pending.Error, loadErr)
					}
				}

				err = ApplySelfUpdate(ctx, stack.DockerCli, ApplySelfOptions{Store: store, JournalID: record.ID, Log: slog.Default()})
			} else {
				err = applySelfDriftProject(ctx, stack.DockerCli, stack.Client, stack.Service,
					project, &record, store, nil, "", slog.Default())
				if mode == "unhealthy" && err != nil {
					err = finishSelfApplyFailure(ctx, stack.Client, store, record, err, slog.Default())
				}
			}

			if err != nil {
				t.Fatalf("%s network update: %v", mode, err)
			}

			if mode == "healthy" {
				for _, name := range []string{"app", "sidecar"} {
					list := selfUpdateStackContainers(ctx, t, stack.Client, stackName, name)
					if len(list) != 1 || list[0].State != container.StateRunning {
						t.Fatalf("%s after network drift: %+v", name, list)
					}

					if list[0].ID == map[string]string{"app": originalApp, "sidecar": originalSidecar}[name] {
						t.Errorf("%s not updated with its new environment", name)
					}

					insp, inspectErr := stack.Client.ContainerInspect(ctx, list[0].ID, client.ContainerInspectOptions{})
					if inspectErr != nil || !slices.Contains(insp.Container.Config.Env, "GENERATION=new") {
						t.Errorf("%s did not receive new configuration: %v", name, inspectErr)
					}
				}
			} else {
				terminal, loadErr := store.Load(record.ID)
				if loadErr != nil || terminal.State != selfupdate.StateRolledBack {
					t.Fatalf("recovery journal = %q, %v; want rolled back", terminal.State, loadErr)
				}

				if mode == "rollback-retry" && terminal.Error != record.Error {
					t.Errorf("rollback error = %q; want original cause %q", terminal.Error, record.Error)
				}

				for _, name := range []string{"app", "sidecar"} {
					list := selfUpdateStackContainers(ctx, t, stack.Client, stackName, name)
					if len(list) != 1 || list[0].State != container.StateRunning {
						t.Fatalf("%s after rollback: %+v", name, list)
					}

					insp, inspectErr := stack.Client.ContainerInspect(ctx, list[0].ID, client.ContainerInspectOptions{})
					if inspectErr != nil || insp.Container.Config.Env == nil {
						t.Fatalf("inspect restored %s: %v", name, inspectErr)
					}

					if !slices.Contains(insp.Container.Config.Env, "GENERATION=old") {
						t.Errorf("%s retained new generation after rollback: %v", name, insp.Container.Config.Env)
					}
				}
			}

			cloneState, inspectErr := stack.Client.ContainerInspect(ctx, clone.ID, client.ContainerInspectOptions{})
			if inspectErr != nil || !cloneState.Container.State.Running {
				t.Errorf("applier did not survive network recreation: %v", inspectErr)
			}

			net, inspectErr := stack.Client.NetworkInspect(ctx, stackName+"_backend", client.NetworkInspectOptions{})
			if inspectErr != nil {
				t.Fatal(inspectErr)
			}

			expected := "new"
			if mode != "healthy" {
				expected = "old"
			}

			if net.Network.Labels["generation"] != expected {
				t.Errorf("network generation = %q, want %q", net.Network.Labels["generation"], expected)
			}
		})
	}
}
