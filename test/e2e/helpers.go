//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/docker"
)

// initRepo creates the repo the gitserver container mounts read-only, plus a
// separate worktree used to build the fixture and later commits. The repo's
// storage lives directly at repoPath (the standard bare-repo layout: HEAD,
// objects/, refs/), so commits are visible to the mounted gitserver
// container immediately, without any push/network step - the same
// end-to-end effect as the legacy run.sh, without shelling out to git.
func (h *Harness) initRepo() {
	h.t.Helper()

	if err := os.MkdirAll(h.repoPath, 0o755); err != nil {
		h.t.Fatalf("create repo dir: %v", err)
	}

	if err := os.MkdirAll(h.worktree, 0o755); err != nil {
		h.t.Fatalf("create worktree dir: %v", err)
	}

	storer := filesystem.NewStorage(osfs.New(h.repoPath), cache.NewObjectLRUDefault())

	repo, err := git.InitWithOptions(storer, osfs.New(h.worktree), git.InitOptions{
		DefaultBranch: plumbing.NewBranchReferenceName("main"),
	})
	if err != nil {
		h.t.Fatalf("init repo: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		h.t.Fatalf("get worktree: %v", err)
	}

	h.repo = repo
	h.wt = wt
}

// SideRepo is a second repository served by the same gitserver container, used
// by scenarios that need more than the one scenario repo - a submodule target,
// for instance.
type SideRepo struct {
	t        *testing.T
	Name     string // repo name without the .git suffix
	URL      string // clone URL as seen from inside the test network
	worktree string
	wt       *git.Worktree
}

// NewSideRepo creates an additional bare repo under the directory the
// gitserver mounts, so it is reachable at http://gitserver/<name>.git.
// Call before Start.
func (h *Harness) NewSideRepo(name string) *SideRepo {
	h.t.Helper()

	repoPath := filepath.Join(h.workDir, "repos", name+".git")
	worktree := filepath.Join(h.workDir, "src", name)

	for _, dir := range []string{repoPath, worktree} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			h.t.Fatalf("create side repo dir %s: %v", dir, err)
		}
	}

	storer := filesystem.NewStorage(osfs.New(repoPath), cache.NewObjectLRUDefault())

	repo, err := git.InitWithOptions(storer, osfs.New(worktree), git.InitOptions{
		DefaultBranch: plumbing.NewBranchReferenceName("main"),
	})
	if err != nil {
		h.t.Fatalf("init side repo %s: %v", name, err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		h.t.Fatalf("get side repo worktree %s: %v", name, err)
	}

	return &SideRepo{
		t:        h.t,
		Name:     name,
		URL:      "http://gitserver/" + name + ".git",
		worktree: worktree,
		wt:       wt,
	}
}

// Write puts files into the side repo worktree, keyed by path relative to its root.
func (s *SideRepo) Write(files map[string]string) {
	s.t.Helper()

	for rel, content := range files {
		path := filepath.Join(s.worktree, rel)

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			s.t.Fatalf("create dir for %s: %v", rel, err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			s.t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// Commit stages and commits everything in the side repo worktree, returning
// the new commit hash - the value a parent repo's gitlink must point at.
func (s *SideRepo) Commit(message string) plumbing.Hash {
	s.t.Helper()

	if _, err := s.wt.Add("."); err != nil {
		s.t.Fatalf("stage side repo changes: %v", err)
	}

	hash, err := s.wt.Commit(message, &git.CommitOptions{
		Author: &object.Signature{Name: "e2e", Email: "e2e@localhost", When: time.Now()},
	})
	if err != nil {
		s.t.Fatalf("commit side repo: %v", err)
	}

	return hash
}

// SetSubmoduleGitlink pins path in the scenario repo to a commit of another
// repository. go-git cannot add a submodule, so the index entry is written
// directly; RepoPush applies it after staging so a plain Add cannot drop it.
// The matching .gitmodules entry still has to be part of the fixture.
func (h *Harness) SetSubmoduleGitlink(path string, commit plumbing.Hash) {
	h.t.Helper()

	if h.gitlinks == nil {
		h.gitlinks = map[string]plumbing.Hash{}
	}

	h.gitlinks[path] = commit
}

func (h *Harness) applyGitlinks() {
	h.t.Helper()

	if len(h.gitlinks) == 0 {
		return
	}

	idx, err := h.repo.Storer.Index()
	if err != nil {
		h.t.Fatalf("read index: %v", err)
	}

	for path, commit := range h.gitlinks {
		entry, err := idx.Entry(path)
		if err != nil {
			entry = idx.Add(path)
		}

		entry.Hash = commit
		entry.Mode = filemode.Submodule
	}

	if err = h.repo.Storer.SetIndex(idx); err != nil {
		h.t.Fatalf("write index: %v", err)
	}
}

func (h *Harness) copyFixture(fixtureDir string) {
	h.t.Helper()

	if err := copyDir(fixtureDir, h.worktree); err != nil {
		h.t.Fatalf("copy fixture: %v", err)
	}
}

// CopyScenarioDir overlays scenarios/<scenario>/<name>/ onto the worktree,
// for a scenario whose later commits add files rather than edit the fixture
// in place. Keeping that content as real files next to "fixture" means an
// encrypted or binary payload stays reviewable and re-generatable, instead of
// living as a string literal in the test.
func (h *Harness) CopyScenarioDir(name string) {
	h.t.Helper()

	dir := filepath.Join(scenarioDir(h.scenario), name)
	if _, err := os.Stat(dir); err != nil {
		h.t.Fatalf("unknown scenario directory %q: %v", name, err)
	}

	if err := copyDir(dir, h.worktree); err != nil {
		h.t.Fatalf("copy scenario directory %s: %v", name, err)
	}
}

// RepoPush stages and commits everything currently in the scenario worktree.
// Since the repo's storage is the same directory the gitserver container
// mounts, the commit is immediately visible to the daemon's next poll - no
// push is needed. Named RepoPush to keep scenario code reading the same way
// it did against the shell harness.
func (h *Harness) RepoPush(message string) {
	h.t.Helper()

	if _, err := h.wt.Add("."); err != nil {
		h.t.Fatalf("stage changes: %v", err)
	}

	h.applyGitlinks()

	_, err := h.wt.Commit(message, &git.CommitOptions{
		Author: &object.Signature{Name: "e2e", Email: "e2e@localhost", When: time.Now()},
	})
	if err != nil {
		h.t.Fatalf("commit: %v", err)
	}
}

// ReplaceInWorktree does an in-place string substitution in a file under the
// scenario worktree, for scenarios that mutate the fixture before committing
// again (e.g. flipping a hook's exit code).
func (h *Harness) ReplaceInWorktree(relPath, old, replacement string) {
	h.t.Helper()

	path := filepath.Join(h.worktree, relPath)

	data, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("read %s: %v", relPath, err)
	}

	updated := strings.ReplaceAll(string(data), old, replacement)
	if updated == string(data) {
		h.t.Fatalf("replacement %q not found in %s", old, relPath)
	}

	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil { //nolint:gosec // path is constructed from a test-controlled worktree dir and caller-supplied relative path
		h.t.Fatalf("write %s: %v", relPath, err)
	}
}

// WaitFor re-runs check every second until it returns true or the timeout hits.
func (h *Harness) WaitFor(timeout time.Duration, desc string, check func() bool) {
	h.t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		if check() {
			h.logf("ok: %s", desc)
			return
		}

		if time.Now().After(deadline) {
			h.t.Fatalf("timed out after %s waiting for: %s", timeout, desc)
		}

		time.Sleep(time.Second)
	}
}

// WaitForLog waits until the daemon's logs contain substr, mirroring
// e2e::wait_for + e2e::daemon_has_log.
func (h *Harness) WaitForLog(substr string, timeout time.Duration) {
	h.t.Helper()
	h.WaitFor(timeout, "daemon log contains \""+substr+"\"", func() bool {
		return h.daemonHasLog(substr)
	})
}

// LogMark returns a byte offset that can be used to restrict later log
// assertions to events emitted after the current phase.
func (h *Harness) LogMark() int {
	h.t.Helper()

	if h.selfUpdate {
		return h.selfLogMark()
	}

	return len(h.daemonLogs())
}

func (h *Harness) WaitForLogAfter(substr string, offset int, timeout time.Duration) {
	h.t.Helper()
	h.WaitFor(timeout, "new daemon log contains \""+substr+"\"", func() bool {
		return strings.Contains(h.logsSince(offset), substr)
	})
}

func (h *Harness) WaitForLogOccurrencesAfter(substr string, offset, count int, timeout time.Duration) {
	h.t.Helper()
	h.WaitFor(timeout, fmt.Sprintf("new daemon logs contain %q %d times", substr, count), func() bool {
		return strings.Count(h.logsSince(offset), substr) >= count
	})
}

// logsSince returns the daemon output written after a mark. In self-update mode
// the "daemon" is a succession of containers, so the mark is per container and
// a new one cannot shift the offset of an older one.
func (h *Harness) logsSince(offset int) string {
	if h.selfUpdate {
		return h.selfLogsSince(offset)
	}

	return logsSince(h.daemonLogs(), offset)
}

func logsSince(logs string, offset int) string {
	if offset <= 0 {
		return logs
	}

	if offset >= len(logs) {
		return ""
	}

	return logs[offset:]
}

func (h *Harness) daemonHasLog(substr string) bool {
	return strings.Contains(h.daemonLogs(), substr)
}

func (h *Harness) daemonLogs() string {
	if h.selfUpdate {
		return h.selfLogs()
	}

	return h.containerLogs(h.daemon)
}

// logsContainer is satisfied by testcontainers.Container; kept narrow so log
// helpers work the same for both the daemon and the gitserver container.
type logsContainer interface {
	Logs(ctx context.Context) (io.ReadCloser, error)
}

func (h *Harness) containerLogs(c logsContainer) string {
	if c == nil {
		return ""
	}

	rc, err := c.Logs(h.ctx)
	if err != nil {
		return ""
	}
	defer rc.Close()

	var buf bytes.Buffer

	_, _ = io.Copy(&buf, rc)

	return buf.String()
}

func (h *Harness) dumpLogs(c logsContainer) {
	h.t.Log(h.containerLogs(c))
}

func (h *Harness) dumpTailLogs(c logsContainer, n int) {
	lines := strings.Split(strings.TrimRight(h.containerLogs(c), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	h.t.Log(strings.Join(lines, "\n"))
}

// ContainerID returns the ID of the running container for the given
// compose project/service (a stack deployed by doco-cd itself), or "" if
// none is running - mirroring e2e::container_id.
func (h *Harness) ContainerID(project, service string) string {
	h.t.Helper()

	return h.containerID(h.docker, h.isSwarmMode(), project, service)
}

// ComposeContainerID returns the running Compose container for a deployment
// that explicitly selected Compose mode on a Swarm-capable Docker daemon.
func (h *Harness) ComposeContainerID(project, service string) string {
	h.t.Helper()

	return h.containerID(h.docker, false, project, service)
}

// RemoteComposeContainerID returns a Compose container from the disposable
// remote Docker context enabled with EnableRemoteContext.
func (h *Harness) RemoteComposeContainerID(project, service string) string {
	h.t.Helper()

	return h.containerID(h.remoteDockerClient(), false, project, service)
}

// SwarmContainerID returns the running Swarm task container for a deployment.
func (h *Harness) SwarmContainerID(project, service string) string {
	h.t.Helper()

	return h.containerID(h.docker, true, project, service)
}

// RemoteContainerID returns a Compose container from the disposable remote
// Docker context enabled with EnableRemoteContext.
func (h *Harness) RemoteContainerID(project, service string) string {
	h.t.Helper()

	return h.containerID(h.remoteDockerClient(), false, project, service)
}

func (h *Harness) ContainerImage(containerID string) string {
	h.t.Helper()

	return h.containerImage(h.docker, containerID)
}

// ContainerRunning reports whether containerID is currently running.
func (h *Harness) ContainerRunning(containerID string) bool {
	h.t.Helper()

	return h.containerRunning(h.docker, containerID)
}

// RemoteContainerRunning reports whether containerID is running in the
// disposable remote Docker context.
func (h *Harness) RemoteContainerRunning(containerID string) bool {
	h.t.Helper()

	return h.containerRunning(h.remoteDockerClient(), containerID)
}

func (h *Harness) containerRunning(dockerClient *client.Client, containerID string) bool {
	h.t.Helper()

	inspect, err := dockerClient.ContainerInspect(h.ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		h.t.Fatalf("inspect container %s: %v", shortContainerID(containerID), err)
	}

	return inspect.Container.State != nil && inspect.Container.State.Running
}

// OneOffContainerID returns the retained Compose one-off container for a
// scheduled job, or "" after doco-cd has completed its cleanup.
func (h *Harness) OneOffContainerID(project, service string) string {
	h.t.Helper()

	return h.oneOffContainerID(h.docker, project, service)
}

// SwarmOneOffServiceID returns the single temporary Swarm one-off service for
// a stack, or "" after doco-cd has completed its cleanup.
func (h *Harness) SwarmOneOffServiceID(stack string) string {
	h.t.Helper()

	f := client.Filters{}.Add("label", "com.docker.stack.namespace="+stack)

	services, err := h.docker.ServiceList(h.ctx, client.ServiceListOptions{Filters: f})
	if err != nil {
		h.t.Fatalf("list one-off services for %s: %v", stack, err)
	}

	var oneOffServices []swarmTypes.Service

	for _, service := range services.Items {
		if service.Spec.Labels[docker.DocoCDJobLabels.JobEphemeral] == "true" {
			oneOffServices = append(oneOffServices, service)
		}
	}

	if len(oneOffServices) == 0 {
		return ""
	}

	if len(oneOffServices) != 1 {
		h.t.Fatalf("found %d temporary one-off services for %s", len(oneOffServices), stack)
	}

	return oneOffServices[0].ID
}

// SwarmServiceHasRunningTask reports whether serviceID has a running task.
func (h *Harness) SwarmServiceHasRunningTask(serviceID string) bool {
	h.t.Helper()

	tasks, err := h.docker.TaskList(h.ctx, client.TaskListOptions{
		Filters: client.Filters{}.Add("service", serviceID),
	})
	if err != nil {
		h.t.Fatalf("list tasks for service %s: %v", serviceID, err)
	}

	for _, task := range tasks.Items {
		// Swarm job tasks are desired to complete even while their status is running.
		if task.Status.State == swarmTypes.TaskStateRunning {
			return true
		}
	}

	return false
}

// RemoteOneOffContainerID returns a retained one-off container from the
// disposable remote Docker context.
func (h *Harness) RemoteOneOffContainerID(project, service string) string {
	h.t.Helper()

	return h.oneOffContainerID(h.remoteDockerClient(), project, service)
}

func (h *Harness) oneOffContainerID(dockerClient *client.Client, project, service string) string {
	h.t.Helper()

	f := client.Filters{}.
		Add("label", "com.docker.compose.project="+project).
		Add("label", "com.docker.compose.service="+service).
		Add("label", docker.DocoCDJobLabels.JobEphemeral+"=true").
		Add("label", docker.DocoCDJobLabels.JobRunID)

	containers, err := dockerClient.ContainerList(h.ctx, client.ContainerListOptions{All: true, Filters: f})
	if err != nil {
		h.t.Fatalf("list one-off containers for %s/%s: %v", project, service, err)
	}

	if len(containers.Items) == 0 {
		return ""
	}

	if len(containers.Items) != 1 {
		h.t.Fatalf("found %d retained one-off containers for %s/%s", len(containers.Items), project, service)
	}

	return containers.Items[0].ID
}

func (h *Harness) RemoteContainerImage(containerID string) string {
	h.t.Helper()

	return h.containerImage(h.remoteDockerClient(), containerID)
}

func (h *Harness) remoteDockerClient() *client.Client {
	if h.remoteDocker == nil {
		h.t.Fatal("remote Docker context is not enabled")
	}

	return h.remoteDocker
}

func (h *Harness) containerImage(dockerClient *client.Client, containerID string) string {
	h.t.Helper()

	inspect, err := dockerClient.ContainerInspect(h.ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		h.t.Fatalf("inspect container %s: %v", shortContainerID(containerID), err)
	}

	if inspect.Container.Config == nil {
		h.t.Fatalf("container %s has no configuration", shortContainerID(containerID))
	}

	return inspect.Container.Config.Image
}

func imageReferenceMatches(got, want string) bool {
	return got == want || (!strings.Contains(want, "@") && strings.HasPrefix(got, want+"@sha256:"))
}

func (h *Harness) containerID(dockerClient *client.Client, swarmMode bool, project, service string) string {
	h.t.Helper()

	f := h.containerFilters(swarmMode, project, service)

	containers, err := dockerClient.ContainerList(h.ctx, client.ContainerListOptions{Filters: f})
	if err != nil {
		h.t.Fatalf("list containers for %s/%s: %v", project, service, err)
	}

	if len(containers.Items) == 0 {
		return ""
	}

	return containers.Items[0].ID
}

func (h *Harness) containerFilters(swarmMode bool, project, service string) client.Filters {
	h.t.Helper()

	f := client.Filters{}
	if swarmMode {
		f = f.
			Add("label", "com.docker.stack.namespace="+project).
			Add("label", "com.docker.swarm.service.name="+project+"_"+service)
	} else {
		f = f.
			Add("label", "com.docker.compose.project="+project).
			Add("label", "com.docker.compose.service="+service)
	}

	return f
}

// WaitForContainerRecreate waits until the container for project/service
// exists and has an ID different from oldID.
func (h *Harness) WaitForContainerRecreate(project, service, oldID string, timeout time.Duration) {
	h.t.Helper()
	h.WaitFor(timeout, project+"/"+service+" recreated", func() bool {
		id := h.ContainerID(project, service)
		return id != "" && id != oldID
	})
}

func (h *Harness) WaitForContainerRemoval(project, service string, timeout time.Duration) {
	h.t.Helper()
	h.WaitFor(timeout, project+"/"+service+" removed", func() bool {
		filters := h.containerFilters(h.isSwarmMode(), project, service)

		containers, err := h.docker.ContainerList(h.ctx, client.ContainerListOptions{All: true, Filters: filters})
		if err != nil {
			h.t.Fatalf("list containers for %s/%s: %v", project, service, err)
		}

		return len(containers.Items) == 0
	})
}

// cleanupStacks removes the Compose and Swarm resources that doco-cd deploys
// for this scenario. Both are cleaned because a local Docker daemon can switch
// modes between test runs.
//
// Stack names are read straight from the scenario's .doco-cd.yml files (the
// top-level "name" field of every document, the same one doco-cd itself uses
// as the compose project name) instead of a separately maintained list, so
// there is a single source of truth for a scenario's stack names.
func (h *Harness) cleanupStacks() {
	for stack := range h.scenarioStackNames() {
		h.removeComposeResources(stack)
		h.removeSwarmStack(stack)
	}
}

func (h *Harness) removeComposeResources(stack string) {
	f := client.Filters{}.Add("label", "com.docker.compose.project="+stack)

	containers, err := h.docker.ContainerList(h.ctx, client.ContainerListOptions{All: true, Filters: f})
	if err != nil {
		h.logf("list compose containers for %s: %v", stack, err)
	} else {
		for _, c := range containers.Items {
			if _, err := h.docker.ContainerRemove(h.ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
				h.logf("remove compose container %s: %v", c.ID, err)
			}
		}
	}

	networks, err := h.docker.NetworkList(h.ctx, client.NetworkListOptions{Filters: f})
	if err != nil {
		h.logf("list compose networks for %s: %v", stack, err)
		return
	}

	for _, network := range networks.Items {
		if _, err := h.docker.NetworkRemove(h.ctx, network.ID, client.NetworkRemoveOptions{}); err != nil {
			h.logf("remove compose network %s: %v", network.Name, err)
		}
	}
}

func (h *Harness) removeSwarmStack(stack string) {
	if !h.isSwarmMode() {
		return
	}

	cmd := exec.CommandContext(h.ctx, "docker", "stack", "rm", stack)
	if out, err := cmd.CombinedOutput(); err != nil {
		h.logf("remove swarm stack %s: %v: %s", stack, err, out)
		return
	}

	f := client.Filters{}.Add("label", "com.docker.stack.namespace="+stack)
	deadline := time.Now().Add(30 * time.Second)

	for {
		containers, containerErr := h.docker.ContainerList(h.ctx, client.ContainerListOptions{All: true, Filters: f})

		networks, networkErr := h.docker.NetworkList(h.ctx, client.NetworkListOptions{Filters: f})
		if containerErr == nil && networkErr == nil && len(containers.Items) == 0 && len(networks.Items) == 0 {
			return
		}

		if time.Now().After(deadline) {
			_, _ = fmt.Fprintf(os.Stderr, "[e2e] timed out waiting for swarm resources for %s to be removed\n", stack)
			return
		}

		time.Sleep(2 * time.Second)
	}
}

func (h *Harness) isSwarmMode() bool {
	result, err := h.docker.Info(h.ctx, client.InfoOptions{})
	if err != nil {
		return false
	}

	return result.Info.Swarm.ControlAvailable
}

// scenarioStackNames walks the scenario's whole directory and collects the
// "name" of every document of every .doco-cd.yml found. Walking past
// "fixture" matters because a scenario's later commits can add stacks, and
// those stacks still need cleaning up.
func (h *Harness) scenarioStackNames() set.Set[string] {
	names := set.New[string]()
	names.Add(h.extraStacks...)

	_ = filepath.WalkDir(scenarioDir(h.scenario), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || d.Name() != ".doco-cd.yml" || !d.Type().IsRegular() {
			return nil
		}

		data, readErr := os.ReadFile(path) //nolint:gosec // path comes from trusted WalkDir over the scenario dir; os.ReadDir basenames exclude traversal
		if readErr != nil {
			return nil //nolint:nilerr // skip unreadable files, continue walking
		}

		names.Add(stackNamesFromConfig(data)...)

		return nil
	})

	return names
}

// stackNamesFromConfig returns the "name" of every document in a deploy
// config, not just the first: one config can declare several stacks and each
// of them needs cleaning up.
func stackNamesFromConfig(data []byte) []string {
	var names []string

	decoder := yaml.NewDecoder(bytes.NewReader(data))

	for {
		var cfg struct {
			Name string `yaml:"name"`
		}

		if decoder.Decode(&cfg) != nil {
			return names
		}

		if cfg.Name != "" {
			names = append(names, cfg.Name)
		}
	}
}

// scenarioDir returns the on-disk directory holding a scenario's fixture and
// any additional content its later commits copy in.
func scenarioDir(scenario string) string {
	return filepath.Join(repoDir, "test", "e2e", "scenarios", scenario)
}

// copyDir recursively copies src into dst (dst must already exist).
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := os.MkdirAll(dstPath, 0o755); err != nil {
				return err
			}

			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}

			continue
		}

		data, err := os.ReadFile(srcPath)
		if err != nil {
			return err
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		if err := os.WriteFile(dstPath, data, info.Mode()); err != nil { //nolint:gosec // dstPath is constructed from os.ReadDir basenames; path traversal is impossible
			return err
		}
	}

	return nil
}
