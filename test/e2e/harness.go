//go:build e2e

// Package e2e runs black-box tests against a real doco-cd instance built
// from the working tree next to a tiny anonymous git server.
// Scenarios drive it the way a user would: commit changes, wait, assert
// on containers and daemon state with the docker client.
package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kimdre/doco-cd/internal/docker"
)

// repoDir is the doco-cd repo root, relative to this package (test/e2e).
const repoDir = "../.."

// Harness owns one gitserver + one doco-cd container built from the working
// tree, plus the host-side git repo the daemon polls. Every scenario gets
// its own instance (own network, own containers, own workdir), so scenarios
// can run in parallel and don't share daemon state.
type Harness struct {
	t        *testing.T
	ctx      context.Context
	scenario string

	workDir    string // host tmp dir: repos/<scenario>.git + src/<scenario>
	repoPath   string // bare repo dir the gitserver container mounts read-only
	worktree   string // host worktree dir used to build fixture/commits
	dataVolume string
	volumes    []string

	wt     *git.Worktree
	docker *client.Client
	net    *testcontainers.DockerNetwork
	gitSrv testcontainers.Container
	daemon testcontainers.Container

	remoteContext    bool
	contextConfigDir string
	remoteDaemon     testcontainers.Container
	remoteDocker     *client.Client

	teardownOnce sync.Once
}

var (
	suiteHarnessesMu sync.Mutex
	suiteHarnesses   []*Harness

	buildGitServerImageOnce = sync.OnceValues(buildGitServerImage)
	buildDaemonImageOnce    = sync.OnceValues(buildDaemonImage)
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
		t:          t,
		ctx:        context.Background(),
		scenario:   scenario,
		workDir:    workDir,
		dataVolume: "doco-cd-e2e-data-" + filepath.Base(workDir),
		docker:     dockerCli,
	}
	h.repoPath = filepath.Join(h.workDir, "repos", scenario+".git")
	h.worktree = filepath.Join(h.workDir, "src", scenario)

	if keepAlive {
		registerSuiteHarness(h)
	} else {
		t.Cleanup(h.teardown)
	}

	t.Cleanup(h.logFailure)

	return h
}

// EnableRemoteContext adds a second disposable Docker daemon exposed to doco-cd
// as the named context "remote". Call before Start.
func (h *Harness) EnableRemoteContext() {
	h.t.Helper()
	h.remoteContext = true
}

// TrackVolume registers a scenario-owned named volume for teardown.
func (h *Harness) TrackVolume(name string) {
	h.t.Helper()
	h.volumes = append(h.volumes, name)
}

// Start creates the initial fixture commit, builds and starts the gitserver
// + doco-cd containers, and waits for the daemon to become healthy.
func (h *Harness) Start() {
	h.t.Helper()

	h.logf("preparing scenario %q", h.scenario)

	fixtureDir := filepath.Join(repoDir, "test", "e2e", "scenarios", h.scenario, "fixture")
	if _, err := os.Stat(fixtureDir); err != nil {
		h.t.Fatalf("unknown scenario %q: %v", h.scenario, err)
	}

	prewarmImages()

	h.initRepo()
	h.copyFixture(fixtureDir)
	h.RepoPush("e2e: initial fixture")

	net, err := tcnetwork.New(h.ctx)
	if err != nil {
		h.t.Fatalf("create network: %v", err)
	}

	h.net = net

	h.logf("starting gitserver")
	h.startGitServer()

	if h.remoteContext {
		h.logf("starting remote Docker daemon")
		h.startRemoteDocker()
		h.writeDockerContextConfig()
	}

	pollPath := h.writePollConfig()
	h.logf("starting daemon")
	h.startDaemon(pollPath)
}

func (h *Harness) startGitServer() {
	h.t.Helper()

	h.logf("building/reusing gitserver image")

	image, err := buildGitServerImageOnce()
	if err != nil {
		h.t.Fatalf("build gitserver image: %v", err)
	}

	h.logf("waiting for gitserver container startup")

	gitSrv, err := testcontainers.GenericContainer(h.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:          image,
			Name:           h.containerName("gitserver"),
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
	h.logContainerStart("gitserver", gitSrv)
}

func (h *Harness) startDaemon(pollConfigPath string) {
	h.t.Helper()

	h.logf("building/reusing daemon image")

	image, err := buildDaemonImageOnce()
	if err != nil {
		h.t.Fatalf("build doco-cd image: %v", err)
	}

	h.logf("waiting for daemon healthcheck")

	daemon, err := testcontainers.GenericContainer(h.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:    image,
			Name:     h.containerName("doco-cd"),
			Networks: []string{h.net.Name},
			Env: map[string]string{
				"TZ":               "Etc/UTC",
				"LOG_LEVEL":        "debug",
				"POLL_CONFIG_FILE": "/config/poll.yaml",
				"DOCKER_CONFIG":    "/root/.docker",
			},
			Mounts: testcontainers.ContainerMounts{
				{Source: testcontainers.GenericVolumeMountSource{Name: h.dataVolume}, Target: "/data"},
			},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.Binds = append(hc.Binds,
					"/var/run/docker.sock:/var/run/docker.sock",
					pollConfigPath+":/config/poll.yaml:ro",
				)
				if h.contextConfigDir != "" {
					hc.Binds = append(hc.Binds, h.contextConfigDir+":/root/.docker:ro")
				}
			},
			WaitingFor: wait.ForExec([]string{"/doco-cd", "healthcheck"}).
				WithStartupTimeout(60 * time.Second).
				WithPollInterval(500 * time.Millisecond),
		},
		Started: true,
	})
	if err != nil {
		h.logf("--- gitserver logs ---")
		h.dumpLogs(h.gitSrv)
		h.t.Fatalf("start doco-cd: %v", err)
	}

	h.daemon = daemon
	h.logContainerStart("doco-cd", daemon)
}

func (h *Harness) startRemoteDocker() {
	h.t.Helper()

	h.logf("waiting for remote Docker API")

	remote, err := testcontainers.GenericContainer(h.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:          docker.TestRemoteDockerImage,
			Name:           h.containerName("remote-docker"),
			Networks:       []string{h.net.Name},
			NetworkAliases: map[string][]string{h.net.Name: {"remote-docker"}},
			Env:            map[string]string{"DOCKER_TLS_CERTDIR": ""},
			Cmd:            []string{"--host=tcp://0.0.0.0:2375"},
			ExposedPorts:   []string{"2375/tcp"},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.Privileged = true
			},
			WaitingFor: wait.ForListeningPort("2375/tcp").
				WithStartupTimeout(90 * time.Second).
				WithPollInterval(500 * time.Millisecond),
		},
		Started: true,
	})
	if err != nil {
		h.t.Fatalf("start remote Docker daemon: %v", err)
	}

	host, err := remote.Host(h.ctx)
	if err != nil {
		_ = remote.Terminate(h.ctx)
		h.t.Fatalf("resolve remote Docker host: %v", err)
	}

	port, err := remote.MappedPort(h.ctx, "2375/tcp")
	if err != nil {
		_ = remote.Terminate(h.ctx)
		h.t.Fatalf("resolve remote Docker port: %v", err)
	}

	remoteDocker, err := client.New(
		client.WithHost("tcp://" + net.JoinHostPort(host, port.Port())),
	)
	if err != nil {
		_ = remote.Terminate(h.ctx)
		h.t.Fatalf("create remote Docker client: %v", err)
	}

	if _, err := remoteDocker.Ping(h.ctx, client.PingOptions{}); err != nil {
		_ = remoteDocker.Close()
		_ = remote.Terminate(h.ctx)
		h.t.Fatalf("ping remote Docker daemon: %v", err)
	}

	h.remoteDaemon = remote
	h.remoteDocker = remoteDocker
	h.logContainerStart("remote-docker", remote)
}

func (h *Harness) writeDockerContextConfig() {
	h.t.Helper()

	configDir := filepath.Join(h.workDir, "docker-config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		h.t.Fatalf("create Docker context config: %v", err)
	}

	cmd := exec.Command("docker", "--config", configDir, "context", "create", "remote",
		"--docker", "host=tcp://remote-docker:2375")
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("create remote Docker context: %v\n%s", err, out)
	}

	h.contextConfigDir = configDir
}

// prewarmImages starts both image builds in the background so repo, fixture
// and network setup overlap with them. Both are sync.OnceValues, so
// startGitServer/startDaemon reuse these results and still report any error.
func prewarmImages() {
	go func() { _, _ = buildGitServerImageOnce() }()
	go func() { _, _ = buildDaemonImageOnce() }()
}

// buildGitServerImage builds the static gitserver image once for the test
// process; scenarios only create containers from the shared image.
func buildGitServerImage() (string, error) {
	image := gitServerImageName()
	cmd := exec.Command("docker", "build", "-t", image, filepath.Join(repoDir, "test", "e2e", "harness", "gitserver"))

	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")

	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%w\n%s", err, out)
	}

	return image, nil
}

func gitServerImageName() string {
	return fmt.Sprintf("doco-cd-e2e-gitserver:%d", os.Getpid())
}

// buildDaemonImage builds the doco-cd image from the working tree once per test
// process via the docker CLI (as opposed to testcontainers FromDockerfile,
// which drives the raw ImageBuild API) and returns its tag.
//
// The binary is compiled on the host and injected as the Dockerfile's `build`
// stage, so the image build skips the in-image Go compile: BuildKit's
// `--mount=type=cache` Go build cache is not exported by `--cache-to`, which
// made that compile a multi-minute, never-cached step on every CI run.
func buildDaemonImage() (string, error) {
	tag := daemonImageName()

	binDir, err := buildDaemonBinary()
	if err != nil {
		return "", err
	}

	defer func() { _ = os.RemoveAll(binDir) }()

	cmd := exec.Command("docker", daemonBuildArgs(tag, binDir)...) // #nosec G204 -- arguments are passed directly, never through a shell.

	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")

	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%w\n%s", err, out)
	}

	return tag, nil
}

// buildDaemonBinary compiles doco-cd into a fresh directory for the platform
// the docker daemon runs containers on. The nobitwarden tag keeps the build
// CGO-free and mirrors the image build's DISABLE_BITWARDEN=true.
func buildDaemonBinary() (string, error) {
	binDir, err := os.MkdirTemp("", "doco-cd-e2e-bin-")
	if err != nil {
		return "", fmt.Errorf("create binary dir: %w", err)
	}

	cmd := exec.Command("go", "build",
		"-tags", "nobitwarden",
		"-ldflags", "-s -w -X github.com/kimdre/doco-cd/internal/config/app.Version=e2e",
		"-o", binDir,
		"./cmd/doco-cd",
	)
	cmd.Dir = repoDir

	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+daemonGoArch())

	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(binDir)

		return "", fmt.Errorf("build doco-cd binary: %w\n%s", err, out)
	}

	return binDir, nil
}

// daemonGoArch reports the GOARCH the docker daemon runs containers with, so
// the host build still produces a runnable binary when the host and the daemon
// differ (e.g. an arm64 host driving an amd64 daemon).
func daemonGoArch() string {
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Arch}}").Output()
	if arch := strings.TrimSpace(string(out)); err == nil && arch != "" {
		return arch
	}

	return runtime.GOARCH
}

func daemonBuildArgs(tag, binDir string) []string {
	args := []string{"build"}

	if scope := os.Getenv("E2E_BUILD_CACHE_SCOPE"); scope != "" {
		args = []string{
			"buildx", "build",
			"--load",
			"--cache-from", "type=gha,scope=" + scope,
			"--cache-to", "type=gha,mode=max,scope=" + scope,
		}
	}

	args = append(args,
		"-t", tag,
		"--provenance", "false", // attestations are dead weight for a throwaway image
		"--build-arg", "DISABLE_BITWARDEN=true", // not exercised by e2e, and the host build is CGO-free
		"--build-context", "build="+binDir, // replaces the Dockerfile's Go build stage with the host-built binary
	)

	return append(args, repoDir)
}

func daemonImageName() string {
	return fmt.Sprintf("doco-cd-e2e:%d", os.Getpid())
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

func (h *Harness) containerName(component string) string {
	return "doco-cd-e2e-" + component + "-" + strings.TrimPrefix(filepath.Base(h.workDir), "doco-cd-e2e-")
}

func (h *Harness) teardown() {
	h.teardownInternal()
}

func (h *Harness) logf(format string, args ...any) {
	h.t.Logf("[e2e] "+format, args...)
}

func (h *Harness) logContainerStart(component string, c testcontainers.Container) {
	name := c.GetContainerID()

	inspect, err := c.Inspect(h.ctx)
	if err == nil && inspect != nil && inspect.Name != "" {
		name = strings.TrimPrefix(inspect.Name, "/")
	}

	h.logf("%s container: %s (%s)", component, name, shortContainerID(c.GetContainerID()))
}

func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}

	return id
}

func (h *Harness) teardownSuite() {
	h.teardownInternal()
}

func (h *Harness) teardownInternal() {
	h.teardownOnce.Do(func() {
		if h.daemon != nil {
			_ = h.daemon.Terminate(h.ctx)
		}

		h.cleanupStacks()

		for _, volume := range h.volumes {
			if _, err := h.docker.VolumeRemove(h.ctx, volume, client.VolumeRemoveOptions{Force: true}); err != nil {
				h.logf("remove named volume %s: %v", volume, err)
			}
		}

		if h.remoteDocker != nil {
			_ = h.remoteDocker.Close()
		}

		if h.remoteDaemon != nil {
			_ = h.remoteDaemon.Terminate(h.ctx)
		}

		if h.gitSrv != nil {
			_ = h.gitSrv.Terminate(h.ctx)
		}

		if h.net != nil {
			_ = h.net.Remove(h.ctx)
		}

		_, _ = h.docker.VolumeRemove(h.ctx, h.dataVolume, client.VolumeRemoveOptions{Force: true})

		_ = os.RemoveAll(h.workDir)
	})
}

func (h *Harness) logFailure() {
	if h.t.Failed() {
		h.logf("--- daemon logs (last 100 lines) ---")
		h.dumpTailLogs(h.daemon, 100)

		if h.remoteDaemon != nil {
			h.logf("--- remote Docker logs (last 100 lines) ---")
			h.dumpTailLogs(h.remoteDaemon, 100)
		}
	}
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
