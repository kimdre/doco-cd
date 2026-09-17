//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kimdre/doco-cd/internal/docker"
)

const (
	// selfBootstrapTimeout bounds the one-shot bootstrap run: clone, load and
	// deploy with images already present on the daemon.
	selfBootstrapTimeout = 3 * time.Minute
	// selfHealthyTimeout bounds how long the first managed container may take
	// to report healthy after the bootstrap created it.
	selfHealthyTimeout = 2 * time.Minute
	// selfLogPollInterval is how often container logs are snapshotted. Short
	// enough to catch an applier that lives a few seconds, long enough not to
	// flood the daemon with one request per container per scenario.
	selfLogPollInterval = 500 * time.Millisecond
	// selfAliveSampleInterval is how often [Harness.WatchAlive] samples a
	// container's state.
	selfAliveSampleInterval = 250 * time.Millisecond
	// selfAliveStopTimeout bounds the wait for the sampler, so a stuck Docker
	// call cannot hang the scenario.
	selfAliveStopTimeout = 30 * time.Second
)

// EnableSelfUpdate makes the harness bring doco-cd up through its own
// `apply-self --bootstrap` path, so the container under test is a real member
// of a compose project that doco-cd reconciles. Call before Start.
func (h *Harness) EnableSelfUpdate(stack, service string) {
	h.t.Helper()

	h.selfUpdate = true
	h.selfStack = stack
	h.selfService = service
	h.selfImageRepo = "doco-cd-e2e-self-" + strings.TrimPrefix(filepath.Base(h.workDir), "doco-cd-e2e-")
	h.selfLogCache = map[string]string{}

	// The collector polls the daemon continuously, so it must stop when this
	// scenario ends. Harnesses otherwise live until suite teardown, and eight
	// collectors polling for the whole run slow every other scenario down.
	h.t.Cleanup(h.stopSelfLogCollector)
}

// SelfImageRepo is the per-scenario image repository the fixture refers to.
func (h *Harness) SelfImageRepo() string {
	return h.selfImageRepo
}

// prepareSelfUpdateFixture tags the freshly built image as v1 and v2 and writes
// the .env the fixture interpolates. Both tags point at the same image, so an
// image bump in the fixture is a pure config change with no rebuild.
func (h *Harness) prepareSelfUpdateFixture() {
	h.t.Helper()

	image, err := buildDaemonImageOnce()
	if err != nil {
		h.t.Fatalf("build doco-cd image: %v", err)
	}

	for _, tag := range []string{"v1", "v2"} {
		ref := h.selfImageRepo + ":" + tag
		if _, err = h.docker.ImageTag(h.ctx, client.ImageTagOptions{Source: image, Target: ref}); err != nil {
			h.t.Fatalf("tag %s: %v", ref, err)
		}
	}

	interval := h.pollInterval
	if interval == 0 {
		interval = 10 * time.Second
	}

	env := fmt.Sprintf(
		"E2E_SELF_IMAGE_REPO=%s\nE2E_NETWORK=%s\nE2E_DATA_VOLUME=%s\nE2E_SCENARIO=%s\nE2E_POLL_INTERVAL=%s\n",
		h.selfImageRepo, h.net.Name, h.dataVolume, h.scenario, interval,
	)

	envPath := filepath.Join(h.worktree, "deploy", ".env")
	if err = os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
		h.t.Fatalf("create fixture deploy dir: %v", err)
	}

	if err = os.WriteFile(envPath, []byte(env), 0o600); err != nil {
		h.t.Fatalf("write fixture .env: %v", err)
	}

	// The data volume must exist before compose attaches to it as external.
	if _, err = h.docker.VolumeCreate(h.ctx, client.VolumeCreateOptions{Name: h.dataVolume}); err != nil {
		h.t.Fatalf("create data volume: %v", err)
	}
}

// startSelfBootstrap runs `apply-self --bootstrap` once. It deploys the stack
// from the repo, which creates container #1 with correct compose and doco-cd
// labels, then exits.
func (h *Harness) startSelfBootstrap(pollConfigPath string) {
	h.t.Helper()

	image, err := buildDaemonImageOnce()
	if err != nil {
		h.t.Fatalf("build doco-cd image: %v", err)
	}

	bootstrap, err := testcontainers.GenericContainer(h.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:    image,
			Name:     h.containerName("bootstrap"),
			Networks: []string{h.net.Name},
			Cmd:      []string{"apply-self", "--bootstrap"},
			Env: map[string]string{
				"TZ":                  "Etc/UTC",
				"LOG_LEVEL":           "debug",
				"POLL_CONFIG_FILE":    "/config/poll.yaml",
				"DOCKER_CONFIG":       "/root/.docker",
				"SELF_UPDATE_ENABLED": "true",
			},
			Mounts: testcontainers.ContainerMounts{
				{Source: testcontainers.GenericVolumeMountSource{Name: h.dataVolume}, Target: "/data"},
			},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.Binds = append(hc.Binds,
					"/var/run/docker.sock:/var/run/docker.sock",
					pollConfigPath+":/config/poll.yaml:ro",
				)
			},
			WaitingFor: wait.ForExit().WithExitTimeout(selfBootstrapTimeout),
		},
		Started: true,
	})
	if err != nil {
		h.logf("--- gitserver logs ---")
		h.dumpLogs(h.gitSrv)

		if bootstrap != nil {
			h.logf("--- bootstrap logs ---")
			h.dumpLogs(bootstrap)
		}

		h.t.Fatalf("run bootstrap: %v", err)
	}

	h.bootstrap = bootstrap
	h.logContainerStart("bootstrap", bootstrap)
	h.startSelfLogCollector()

	if code := h.BootstrapExitCode(); code != 0 {
		h.logf("--- cloned repo ---")
		h.logf("%s", h.inspectDataVolume("find /data -maxdepth 4 -not -path '*/.git/*' | head -50"))
		h.logf("--- bootstrap logs ---")
		h.dumpLogs(bootstrap)
		h.t.Fatalf("bootstrap exited with code %d", code)
	}

	h.WaitFor(selfHealthyTimeout, "the managed doco-cd container is healthy", func() bool {
		all := h.SelfContainers(false)

		return len(all) == 1 && h.ContainerHealthy(all[0].ID)
	})
}

// BootstrapExitCode returns the exit code of the one-shot bootstrap container.
func (h *Harness) BootstrapExitCode() int {
	h.t.Helper()

	if h.bootstrap == nil {
		h.t.Fatal("no bootstrap container, call EnableSelfUpdate before Start")
	}

	state, err := h.bootstrap.State(h.ctx)
	if err != nil {
		h.t.Fatalf("inspect bootstrap container: %v", err)
	}

	return state.ExitCode
}

// BootstrapLogs returns the bootstrap container's output.
func (h *Harness) BootstrapLogs() string {
	return h.containerLogs(h.bootstrap)
}

// SelfContainers lists the containers of the self service. Pass all=true to
// include stopped ones, which is what a leftover assertion needs.
func (h *Harness) SelfContainers(all bool) []container.Summary {
	h.t.Helper()

	list, err := h.docker.ContainerList(h.ctx, client.ContainerListOptions{
		All: all,
		Filters: make(client.Filters).
			Add("label", "com.docker.compose.project="+h.selfStack).
			Add("label", "com.docker.compose.service="+h.selfService),
	})
	if err != nil {
		h.t.Fatalf("list self containers: %v", err)
	}

	items := list.Items
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })

	return items
}

// SelfContainerID returns the single running self container, or "" when the
// count is not exactly one.
func (h *Harness) SelfContainerID() string {
	h.t.Helper()

	running := h.SelfContainers(false)
	if len(running) != 1 {
		return ""
	}

	return running[0].ID
}

// SelfAppliers lists the throwaway applier containers for this stack.
func (h *Harness) SelfAppliers(all bool) []container.Summary {
	h.t.Helper()

	list, err := h.docker.ContainerList(h.ctx, client.ContainerListOptions{
		All:     all,
		Filters: make(client.Filters).Add("label", docker.SelfStackLabel+"="+h.selfStack),
	})
	if err != nil {
		h.t.Fatalf("list applier containers: %v", err)
	}

	return list.Items
}

// ContainerLabels returns a container's labels.
func (h *Harness) ContainerLabels(containerID string) map[string]string {
	h.t.Helper()

	result, err := h.docker.ContainerInspect(h.ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		h.t.Fatalf("inspect %s: %v", containerID, err)
	}

	return result.Container.Config.Labels
}

// ContainerHealthy reports whether a container is running and reports healthy.
func (h *Harness) ContainerHealthy(containerID string) bool {
	result, err := h.docker.ContainerInspect(h.ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return false
	}

	state := result.Container.State
	if state == nil || !state.Running {
		return false
	}

	if state.Health == nil {
		return true
	}

	return state.Health.Status == container.Healthy
}

// ContainerExitCode returns a container's exit code and whether it has stopped.
func (h *Harness) ContainerExitCode(containerID string) (int, bool) {
	result, err := h.docker.ContainerInspect(h.ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return 0, false
	}

	state := result.Container.State
	if state == nil || state.Running {
		return 0, false
	}

	return state.ExitCode, true
}

// RepoHead returns the current commit SHA of the scenario repository.
func (h *Harness) RepoHead() string {
	h.t.Helper()

	repo, err := git.PlainOpen(h.repoPath)
	if err != nil {
		h.t.Fatalf("open repo: %v", err)
	}

	ref, err := repo.Head()
	if err != nil {
		h.t.Fatalf("read repo head: %v", err)
	}

	return ref.Hash().String()
}

// WatchAlive samples a container and records when it stopped running. That is
// the zero-downtime claim for a compose handover: the old container must keep
// running until it has drained.
//
// Docker's health verdict is deliberately not the failure signal. On a loaded
// daemon an interval-based healthcheck flaps, which says something about the
// test machine rather than about the handover, so flaps are only reported.
//
// Nothing in here may call t.Fatalf: from a non-test goroutine that is a
// runtime.Goexit, which would kill the sampler silently and leave the stop
// closure waiting forever.
func (h *Harness) WatchAlive(containerID string) func() []time.Time {
	stop := make(chan struct{})
	done := make(chan []time.Time, 1)

	go func() {
		var (
			notRunning []time.Time
			flaps      int
		)

		defer func() {
			if flaps > 0 {
				h.logf("note: %s reported unhealthy in %d samples while still running",
					shortContainerID(containerID), flaps)
			}

			done <- notRunning
		}()

		for {
			select {
			case <-stop:
				return
			case <-time.After(selfAliveSampleInterval):
				running, healthy, ok := h.sampleContainer(containerID)
				switch {
				case !ok:
					// A transient daemon error is not evidence of downtime.
					continue
				case !running:
					notRunning = append(notRunning, time.Now())
				case !healthy:
					flaps++
				}
			}
		}
	}()

	return func() []time.Time {
		close(stop)

		select {
		case failures := <-done:
			return failures
		case <-time.After(selfAliveStopTimeout):
			h.logf("warning: the liveness sampler for %s did not stop in time", shortContainerID(containerID))

			return nil
		}
	}
}

// sampleContainer reports whether a container is running and healthy. The third
// value is false when the daemon could not be asked, which is not a verdict.
func (h *Harness) sampleContainer(containerID string) (running, healthy, ok bool) {
	result, err := h.docker.ContainerInspect(h.ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return false, false, false
	}

	state := result.Container.State
	if state == nil {
		return false, false, true
	}

	if !state.Running {
		return false, false, true
	}

	if state.Health == nil {
		return true, true, true
	}

	return true, state.Health.Status == container.Healthy, true
}

// ContainerExists reports whether a container is still known to the daemon.
func (h *Harness) ContainerExists(containerID string) bool {
	_, err := h.docker.ContainerInspect(h.ctx, containerID, client.ContainerInspectOptions{})

	return err == nil
}

// LogCountAfter counts occurrences of substr in the logs written since mark.
func (h *Harness) LogCountAfter(substr string, mark int) int {
	return strings.Count(h.logsSince(mark), substr)
}

// selfLogMark snapshots the current log length of every known container and
// returns the index of that snapshot.
func (h *Harness) selfLogMark() int {
	h.refreshSelfLogs()

	snapshot := h.snapshotSelfLogs()

	mark := make(map[string]int, len(snapshot))
	for id, logs := range snapshot {
		mark[id] = len(logs)
	}

	h.selfLogMarks = append(h.selfLogMarks, mark)

	return len(h.selfLogMarks) - 1
}

// selfLogsSince concatenates, per container, only what was written after the
// mark. A container that did not exist at mark time contributes its whole log.
func (h *Harness) selfLogsSince(mark int) string {
	h.refreshSelfLogs()

	if mark < 0 || mark >= len(h.selfLogMarks) {
		return h.selfLogs()
	}

	marks := h.selfLogMarks[mark]
	snapshot := h.snapshotSelfLogs()

	ids := make([]string, 0, len(snapshot))
	for id := range snapshot {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	var sb strings.Builder

	for _, id := range ids {
		logs := snapshot[id]

		offset := marks[id]
		if offset >= len(logs) {
			continue
		}

		sb.WriteString(logs[offset:])
	}

	return sb.String()
}

// selfLogs concatenates the logs of every container that has acted as this
// scenario's doco-cd: the bootstrap, each self container and each applier.
// Logs of removed containers stay in the cache, so a mark taken before a
// handover keeps pointing at the same position afterwards.
func (h *Harness) selfLogs() string {
	h.refreshSelfLogs()

	logs := h.snapshotSelfLogs()

	cached := make([]string, 0, len(logs))
	for id := range logs {
		cached = append(cached, id)
	}

	sort.Strings(cached)

	var sb strings.Builder

	for _, id := range cached {
		sb.WriteString(logs[id])
	}

	return sb.String()
}

// refreshSelfLogs re-reads the logs of every container that has acted as this
// scenario's doco-cd. Removed containers keep their last snapshot, so a mark
// taken before a handover still resolves afterwards.
func (h *Harness) refreshSelfLogs() {
	ids := make([]string, 0, 8)

	if h.bootstrap != nil {
		if id := h.bootstrap.GetContainerID(); id != "" {
			ids = append(ids, id)
		}
	}

	ids = append(ids, h.listSelfContainerIDs()...)

	for _, id := range ids {
		logs := h.rawContainerLogs(id)
		if logs == "" {
			continue
		}

		h.selfLogMu.Lock()
		h.selfLogCache[id] = logs
		h.selfLogMu.Unlock()
	}
}

// listSelfContainerIDs lists every container of the self service and every
// applier, without failing the test: it also runs from the log collector
// goroutine, where a Fatalf would be illegal.
func (h *Harness) listSelfContainerIDs() []string {
	filters := []client.Filters{
		make(client.Filters).
			Add("label", "com.docker.compose.project="+h.selfStack).
			Add("label", "com.docker.compose.service="+h.selfService),
		make(client.Filters).Add("label", docker.SelfStackLabel+"="+h.selfStack),
	}

	ids := make([]string, 0, 8)

	for _, f := range filters {
		list, err := h.docker.ContainerList(h.ctx, client.ContainerListOptions{All: true, Filters: f})
		if err != nil {
			continue
		}

		for _, c := range list.Items {
			ids = append(ids, c.ID)
		}
	}

	return ids
}

// startSelfLogCollector snapshots container logs continuously. A handover
// deletes containers within seconds, and their output is the only record of
// what happened, so polling it lazily loses it under load.
//
// [EnableSelfUpdate] registers the stop, so the polling ends with the scenario
// rather than with the suite.
func (h *Harness) startSelfLogCollector() {
	h.selfLogStop = make(chan struct{})
	h.selfLogDone = make(chan struct{})

	stop := h.selfLogStop

	go func() {
		defer close(h.selfLogDone)

		for {
			select {
			case <-stop:
				h.refreshSelfLogs()

				return
			case <-time.After(selfLogPollInterval):
				h.refreshSelfLogs()
			}
		}
	}()
}

// stopSelfLogCollector is safe to call twice: the scenario's t.Cleanup and the
// suite teardown both reach it.
func (h *Harness) stopSelfLogCollector() {
	h.selfLogMu.Lock()

	stop := h.selfLogStop
	h.selfLogStop = nil

	h.selfLogMu.Unlock()

	if stop == nil {
		return
	}

	close(stop)
	<-h.selfLogDone
}

// snapshotSelfLogs returns a stable copy of the cache.
func (h *Harness) snapshotSelfLogs() map[string]string {
	h.selfLogMu.Lock()
	defer h.selfLogMu.Unlock()

	out := make(map[string]string, len(h.selfLogCache))
	for id, logs := range h.selfLogCache {
		out[id] = logs
	}

	return out
}

// rawContainerLogs reads a container's logs by ID, tolerating a removed one.
func (h *Harness) rawContainerLogs(containerID string) string {
	rc, err := h.docker.ContainerLogs(h.ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		return ""
	}

	defer rc.Close() // nolint:errcheck

	var stdout, stderr bytes.Buffer

	// The docker log stream is multiplexed unless the container has a TTY, so
	// the frame headers must be stripped before the text is searchable.
	if _, err = stdcopy.StdCopy(&stdout, &stderr, rc); err != nil {
		return stdout.String() + stderr.String()
	}

	return stdout.String() + stderr.String()
}

// dumpSelfState prints everything a failing self-update scenario needs: the
// stack's containers, their logs, and the handover journal.
func (h *Harness) dumpSelfState() {
	h.logf("--- self containers ---")

	for _, c := range h.SelfContainers(true) {
		h.logf("%s names=%v state=%s image=%s number=%s",
			shortContainerID(c.ID), c.Names, c.State, c.Image,
			c.Labels["com.docker.compose.container-number"])
	}

	h.logf("--- applier containers ---")

	for _, c := range h.SelfAppliers(true) {
		h.logf("%s names=%v state=%s", shortContainerID(c.ID), c.Names, c.State)
		h.logf("--- applier logs ---")
		h.logf("%s", h.rawContainerLogs(c.ID))
	}

	if h.bootstrap != nil {
		h.logf("--- bootstrap logs ---")
		h.dumpTailLogs(h.bootstrap, 100)
	}

	for _, c := range h.SelfContainers(true) {
		h.logf("--- logs of %s (last 60 lines) ---", shortContainerID(c.ID))

		lines := strings.Split(strings.TrimRight(h.rawContainerLogs(c.ID), "\n"), "\n")
		if len(lines) > 60 {
			lines = lines[len(lines)-60:]
		}

		h.logf("%s", strings.Join(lines, "\n"))
	}

	h.logf("--- self-update journal ---")
	h.logf("%s", h.readJournal())
}

// readJournal reads the handover journal off the data volume with a throwaway
// container, since the volume is not reachable from the test host.
func (h *Harness) readJournal() string {
	return h.inspectDataVolume("ls -la /data/self-update 2>/dev/null; cat /data/self-update/*.json 2>/dev/null")
}

// inspectDataVolume runs a shell command against the scenario data volume from
// a throwaway container, since the volume is not reachable from the test host.
func (h *Harness) inspectDataVolume(script string) string {
	created, err := h.docker.ContainerCreate(h.ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: "alpine:3.22",
			Cmd:   []string{"sh", "-c", script},
		},
		HostConfig: &container.HostConfig{
			Binds:      []string{h.dataVolume + ":/data:ro"},
			AutoRemove: false,
		},
	})
	if err != nil {
		return "unavailable: " + err.Error()
	}

	defer func() {
		_, _ = h.docker.ContainerRemove(h.ctx, created.ID, client.ContainerRemoveOptions{Force: true})
	}()

	if _, err = h.docker.ContainerStart(h.ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "unavailable: " + err.Error()
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, stopped := h.ContainerExitCode(created.ID); stopped {
			break
		}

		time.Sleep(200 * time.Millisecond)
	}

	return h.rawContainerLogs(created.ID)
}

// cleanupSelfUpdate removes everything a self-update scenario can leave behind:
// the appliers, the bootstrap container, and any stopped predecessor. Leftovers
// hold the fixture's container_name and would break the next run.
func (h *Harness) cleanupSelfUpdate() {
	if h.bootstrap != nil {
		h.terminateContainer(h.bootstrap)
	}

	filters := []client.Filters{
		make(client.Filters).Add("label", docker.SelfStackLabel+"="+h.selfStack),
		make(client.Filters).Add("label", "com.docker.compose.project="+h.selfStack),
	}

	for _, f := range filters {
		list, err := h.docker.ContainerList(h.ctx, client.ContainerListOptions{All: true, Filters: f})
		if err != nil {
			h.logf("list containers for cleanup: %v", err)
			continue
		}

		for _, c := range list.Items {
			if _, err = h.docker.ContainerRemove(h.ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
				h.logf("remove container %s: %v", shortContainerID(c.ID), err)
			}
		}
	}

	for _, tag := range []string{"v1", "v2"} {
		if _, err := h.docker.ImageRemove(h.ctx, h.selfImageRepo+":"+tag, client.ImageRemoveOptions{}); err != nil {
			h.logf("remove image tag %s: %v", tag, err)
		}
	}
}

// assertNoSelfLeftovers fails the test when the stack did not settle on exactly
// one container. A leak is a bug, not something to clean up quietly.
func (h *Harness) assertNoSelfLeftovers() {
	h.t.Helper()

	if n := len(h.SelfContainers(true)); n != 1 {
		h.t.Errorf("stack has %d containers of service %q at rest, want 1", n, h.selfService)
	}

	if n := len(h.SelfAppliers(true)); n != 0 {
		h.t.Errorf("stack has %d applier containers at rest, want 0", n)
	}
}

// SelfImageID resolves one of the scenario's image tags to its image ID.
func (h *Harness) SelfImageID(tag string) string {
	h.t.Helper()

	result, err := h.docker.ImageInspect(h.ctx, h.selfImageRepo+":"+tag)
	if err != nil {
		h.t.Fatalf("inspect image %s:%s: %v", h.selfImageRepo, tag, err)
	}

	return result.ID
}

// RunsSelfImage reports whether a container runs the given scenario image tag.
// A container restored from a snapshot is pinned to the image ID rather than
// the tag, so both spellings count as a match.
func (h *Harness) RunsSelfImage(containerID, tag string) bool {
	h.t.Helper()

	got := h.ContainerImage(containerID)

	return strings.HasSuffix(got, ":"+tag) || got == h.SelfImageID(tag)
}
