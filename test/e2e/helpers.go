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
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/moby/moby/client"
	"go.yaml.in/yaml/v4"
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

	h.wt = wt
}

func (h *Harness) copyFixture(fixtureDir string) {
	h.t.Helper()

	if err := copyDir(fixtureDir, h.worktree); err != nil {
		h.t.Fatalf("copy fixture: %v", err)
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

// WaitFor re-runs check every 2s until it returns true or the timeout hits,
// mirroring e2e::wait_for from lib.sh.
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

		time.Sleep(2 * time.Second)
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

func (h *Harness) daemonHasLog(substr string) bool {
	return strings.Contains(h.daemonLogs(), substr)
}

func (h *Harness) daemonLogs() string {
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

	f := client.Filters{}
	if h.isSwarmMode() {
		f = f.
			Add("label", "com.docker.stack.namespace="+project).
			Add("label", "com.docker.swarm.service.name="+project+"_"+service)
	} else {
		f = f.
			Add("label", "com.docker.compose.project="+project).
			Add("label", "com.docker.compose.service="+service)
	}

	containers, err := h.docker.ContainerList(h.ctx, client.ContainerListOptions{Filters: f})
	if err != nil {
		h.t.Fatalf("list containers for %s/%s: %v", project, service, err)
	}

	if len(containers.Items) == 0 {
		return ""
	}

	return containers.Items[0].ID
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

// cleanupStacks removes the Compose and Swarm resources that doco-cd deploys
// for this scenario. Both are cleaned because a local Docker daemon can switch
// modes between test runs.
//
// Stack names are read straight from the fixture's .doco-cd.yml files
// (their top-level "name" field, the same one doco-cd itself uses as the
// compose project name) instead of a separately maintained list, so there is
// a single source of truth for a scenario's stack names.
func (h *Harness) cleanupStacks() {
	for _, stack := range h.fixtureStackNames() {
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

// fixtureStackNames walks the scenario's fixture directory and collects the
// "name" field of every .doco-cd.yml found, in case a fixture defines
// multiple deploy targets.
func (h *Harness) fixtureStackNames() []string {
	fixtureDir := filepath.Join(repoDir, "test", "e2e", "scenarios", h.scenario, "fixture")

	var names []string

	_ = filepath.WalkDir(fixtureDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || d.Name() != ".doco-cd.yml" || !d.Type().IsRegular() {
			return nil
		}

		data, readErr := os.ReadFile(path) //nolint:gosec // path comes from trusted WalkDir over the fixture dir; os.ReadDir basenames exclude traversal
		if readErr != nil {
			return nil //nolint:nilerr // skip unreadable files, continue walking
		}

		var cfg struct {
			Name string `yaml:"name"`
		}

		if yaml.Unmarshal(data, &cfg) == nil && cfg.Name != "" {
			names = append(names, cfg.Name)
		}

		return nil
	})

	return names
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
