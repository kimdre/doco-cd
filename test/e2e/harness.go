//go:build e2e

// Package e2e runs black-box tests against a real doco-cd instance built
// from the working tree, using testcontainers-go to start it next to a tiny
// anonymous git server. Scenarios drive it the way a user would: commit
// changes, wait, assert on containers and daemon state with the docker
// client - the same approach as the legacy shell harness in run.sh/lib.sh,
// ported to Go. Repo state is written directly with go-git rather than
// shelling out to the git binary.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// repoDir is the doco-cd repo root, relative to this package (test/e2e).
const repoDir = "../.."

// Harness owns one gitserver + one doco-cd container built from the working
// tree, plus the host-side git repo the daemon polls. Every scenario gets
// its own instance (own network, own containers, own workdir), so scenarios
// can run in parallel and don't share daemon state.
type Harness struct {
	t         *testing.T
	ctx       context.Context
	scenario  string
	keepAlive bool

	workDir  string // host tmp dir: repos/<scenario>.git + src/<scenario>
	repoPath string // bare repo dir the gitserver container mounts read-only
	worktree string // host worktree dir used to build fixture/commits

	repo   *git.Repository
	wt     *git.Worktree
	docker *client.Client
	net    *testcontainers.DockerNetwork
	gitSrv testcontainers.Container
	daemon testcontainers.Container

	teardownOnce sync.Once
}

var (
	suiteHarnessesMu sync.Mutex
	suiteHarnesses   []*Harness
)

// NewHarness prepares a harness for the given scenario. Call Start to bring
// up the gitserver + doco-cd containers.
func NewHarness(t *testing.T, scenario string) *Harness {
	t.Helper()

	dockerCli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}

	workDir, err := os.MkdirTemp("", "doco-cd-e2e-"+scenario+"-")
	if err != nil {
		t.Fatalf("create temp work dir: %v", err)
	}

	keepAlive := keepComponentsAcrossSuite()

	h := &Harness{
		t:         t,
		ctx:       context.Background(),
		scenario:  scenario,
		keepAlive: keepAlive,
		workDir:   workDir,
		docker:    dockerCli,
	}
	h.repoPath = filepath.Join(h.workDir, "repos", scenario+".git")
	h.worktree = filepath.Join(h.workDir, "src", scenario)

	if keepAlive {
		registerSuiteHarness(h)
	} else {
		t.Cleanup(h.teardown)
	}

	return h
}

// Start creates the initial fixture commit, builds and starts the gitserver
// + doco-cd containers, and waits for the daemon to become healthy.
func (h *Harness) Start() {
	h.t.Helper()

	fixtureDir := filepath.Join(repoDir, "test", "e2e", "scenarios", h.scenario, "fixture")
	if _, err := os.Stat(fixtureDir); err != nil {
		h.t.Fatalf("unknown scenario %q: %v", h.scenario, err)
	}

	h.initRepo()
	h.copyFixture(fixtureDir)
	h.RepoPush("e2e: initial fixture")

	net, err := tcnetwork.New(h.ctx)
	if err != nil {
		h.t.Fatalf("create network: %v", err)
	}

	h.net = net

	h.startGitServer()

	pollPath := h.writePollConfig()
	h.startDaemon(pollPath)
}

func (h *Harness) startGitServer() {
	h.t.Helper()

	gitSrv, err := testcontainers.GenericContainer(h.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    filepath.Join(repoDir, "test", "e2e", "harness", "gitserver"),
				Dockerfile: "Dockerfile",
			},
			Networks:       []string{h.net.Name},
			NetworkAliases: map[string][]string{h.net.Name: {"gitserver"}},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.Binds = append(hc.Binds, filepath.Join(h.workDir, "repos")+":/repos")
			},
			WaitingFor: wait.ForListeningPort("80/tcp").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		h.t.Fatalf("start gitserver: %v", err)
	}

	h.gitSrv = gitSrv
}

func (h *Harness) startDaemon(pollConfigPath string) {
	h.t.Helper()

	image := h.buildDaemonImage()

	daemon, err := testcontainers.GenericContainer(h.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:    image,
			Networks: []string{h.net.Name},
			Env: map[string]string{
				"TZ":               "Etc/UTC",
				"LOG_LEVEL":        "debug",
				"POLL_CONFIG_FILE": "/config/poll.yaml",
			},
			Mounts: testcontainers.ContainerMounts{
				{Source: testcontainers.GenericVolumeMountSource{Name: "doco-cd-e2e-data-" + h.scenario}, Target: "/data"},
			},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.Binds = append(hc.Binds,
					"/var/run/docker.sock:/var/run/docker.sock",
					pollConfigPath+":/config/poll.yaml:ro",
				)
			},
			WaitingFor: wait.ForExec([]string{"/doco-cd", "healthcheck"}).
				WithStartupTimeout(60 * time.Second).
				WithPollInterval(2 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		h.logf("--- gitserver logs ---")
		h.dumpLogs(h.gitSrv)
		h.t.Fatalf("start doco-cd: %v", err)
	}

	h.daemon = daemon
}

// buildDaemonImage builds the doco-cd image from the working tree via the
// docker CLI (as opposed to testcontainers' FromDockerfile, which drives the
// raw ImageBuild API and can't establish the BuildKit session the
// Dockerfile's --mount=type=cache instructions require) and returns its tag.
func (h *Harness) buildDaemonImage() string {
	h.t.Helper()

	tag := "doco-cd-e2e:" + h.scenario

	cmd := exec.CommandContext(h.ctx, "docker", "build",
		"-t", tag,
		"--build-arg", "DISABLE_BITWARDEN=true", // not exercised by e2e, skipping it halves the build
		repoDir,
	)

	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")

	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("build doco-cd image: %v\n%s", err, out)
	}

	return tag
}

func (h *Harness) writePollConfig() string {
	h.t.Helper()

	content := fmt.Sprintf("- url: http://gitserver/%s.git\n  reference: refs/heads/main\n  interval: 10\n", h.scenario)

	path := filepath.Join(h.workDir, "poll.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		h.t.Fatalf("write poll config: %v", err)
	}

	return path
}

func (h *Harness) teardown() {
	h.teardownInternal(true)
}

func (h *Harness) logf(format string, args ...any) {
	h.t.Logf("[e2e] "+format, args...)
}

func (h *Harness) teardownSuite() {
	h.teardownInternal(false)
}

func (h *Harness) teardownInternal(includeFailureLogs bool) {
	h.teardownOnce.Do(func() {
		if includeFailureLogs && h.t != nil && h.t.Failed() {
			h.logf("--- daemon logs (last 100 lines) ---")
			h.dumpTailLogs(h.daemon, 100)
		}

		if h.daemon != nil {
			_ = h.daemon.Terminate(h.ctx)
		}

		if h.gitSrv != nil {
			_ = h.gitSrv.Terminate(h.ctx)
		}

		if h.net != nil {
			_ = h.net.Remove(h.ctx)
		}

		_, _ = h.docker.VolumeRemove(h.ctx, "doco-cd-e2e-data-"+h.scenario, client.VolumeRemoveOptions{Force: true})

		h.cleanupStacks()
		_ = os.RemoveAll(h.workDir)
	})
}

func keepComponentsAcrossSuite() bool {
	return os.Getenv("E2E_KEEP_COMPONENTS_RUNNING") != "0"
}

func registerSuiteHarness(h *Harness) {
	suiteHarnessesMu.Lock()
	defer suiteHarnessesMu.Unlock()

	suiteHarnesses = append(suiteHarnesses, h)
}

func teardownSuiteHarnesses() {
	suiteHarnessesMu.Lock()

	harnesses := append([]*Harness(nil), suiteHarnesses...)
	suiteHarnesses = nil
	suiteHarnessesMu.Unlock()

	for i := len(harnesses) - 1; i >= 0; i-- {
		harnesses[i].teardownSuite()
	}
}
