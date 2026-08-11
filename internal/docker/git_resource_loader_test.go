package docker

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"

	"github.com/kimdre/doco-cd/internal/config/app"
)

var gitProtocolMu sync.Mutex

type repositoryLoader struct {
	store storer.Storer
}

func (l repositoryLoader) Load(_ *transport.Endpoint) (storer.Storer, error) {
	return l.store, nil
}

func TestGitResourceLoaderLoadsComposeFromLocalGoGitRepository(t *testing.T) {
	repo := createGitIncludeRepository(t)

	restoreTransport := installLocalGitTransport(t, repo)
	defer restoreTransport()

	cacheBase := t.TempDir()
	resource := "git://example.test/repo.git#main:docker"
	loader := newGitResourceLoader(gitResourceLoaderConfig{CacheBase: cacheBase})

	if !loader.Accept(resource) {
		t.Fatalf("expected loader to accept %q", resource)
	}

	loaded, err := loader.Load(context.Background(), resource)
	if err != nil {
		t.Fatalf("load git include: %v", err)
	}

	if want := filepath.Join("docker", "docker-compose.yml"); filepath.Base(loaded) != "docker-compose.yml" ||
		filepath.Base(filepath.Dir(loaded)) != filepath.Base(filepath.Dir(want)) {
		t.Fatalf("expected default compose file in docker directory, got %q", loaded)
	}

	if _, err := os.Stat(loaded); err != nil {
		t.Fatalf("expected loaded compose file to exist: %v", err)
	}

	if got := loader.Dir(resource); got != filepath.Dir(loaded) {
		t.Fatalf("expected resource directory %q, got %q", filepath.Dir(loaded), got)
	}

	// Compose looks up the working directory by the local path returned by Load.
	if got := loader.Dir(loaded); got != filepath.Dir(loaded) {
		t.Fatalf("expected loaded path directory %q, got %q", filepath.Dir(loaded), got)
	}

	cacheInfo, err := os.Stat(filepath.Join(cacheBase, gitIncludeCacheDirectory))
	if err != nil {
		t.Fatalf("stat cache: %v", err)
	}

	if cacheInfo.Mode().Perm() != 0o700 {
		t.Fatalf("expected secure cache permissions 0700, got %o", cacheInfo.Mode().Perm())
	}
}

func TestGitResourceLoaderRejectsEscapingSubpaths(t *testing.T) {
	repoPath := t.TempDir()
	if _, err := gitIncludePath(repoPath, "../outside"); err == nil {
		t.Fatal("expected path traversal to be rejected")
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repoPath, "escape")); err != nil {
		t.Fatalf("create escape symlink: %v", err)
	}

	if _, err := gitIncludePath(repoPath, "escape"); err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
}

func TestGitResourceLoaderAcceptsDockerComposeGitReferences(t *testing.T) {
	loader := newGitResourceLoader(gitResourceLoaderConfig{CacheBase: t.TempDir()})
	for _, resource := range []string{
		"https://github.com/immich-app/immich.git#v3.0.3:docker/docker-compose.yml",
		"git@github.com:user/repo.git#main:path",
	} {
		if !loader.Accept(resource) {
			t.Errorf("expected loader to accept %q", resource)
		}
	}
}

func TestNewRemoteResourceLoadersWithoutDockerCLIIncludesGitOnly(t *testing.T) {
	loaders := newRemoteResourceLoaders(&app.Config{DataMountPath: t.TempDir()}, nil)
	if len(loaders) != 1 {
		t.Fatalf("expected only the Git loader without a Docker CLI, got %d loaders", len(loaders))
	}

	if _, ok := loaders[0].(*gitResourceLoader); !ok {
		t.Fatalf("expected Git resource loader, got %T", loaders[0])
	}
}

func TestLoadComposeWithGitIncludeWithoutDockerCLI(t *testing.T) {
	repo := createGitIncludeRepository(t)

	restoreTransport := installLocalGitTransport(t, repo)
	defer restoreTransport()

	workingDirectory := t.TempDir()
	t.Setenv("DATA_MOUNT_PATH", t.TempDir())
	t.Setenv("WEBHOOK_SECRET", "test-secret")

	rootComposePath := filepath.Join(workingDirectory, "compose.yaml")

	rootCompose := "include:\n  - path: git://example.test/repo.git#main:docker\n"
	if err := os.WriteFile(rootComposePath, []byte(rootCompose), 0o600); err != nil {
		t.Fatalf("write root compose file: %v", err)
	}

	project, err := LoadCompose(
		context.Background(),
		nil,
		workingDirectory,
		workingDirectory,
		"git-include",
		[]string{rootComposePath},
		nil,
		nil,
		map[string]string{},
	)
	if err != nil {
		t.Fatalf("load compose with Git include: %v", err)
	}

	if _, err := project.GetService("included"); err != nil {
		t.Fatalf("expected service from Git include: %v", err)
	}
}

func createGitIncludeRepository(t *testing.T) *gogit.Repository {
	t.Helper()

	repoPath := t.TempDir()

	repo, err := gogit.PlainInit(repoPath, false)
	if err != nil {
		t.Fatalf("initialize repository: %v", err)
	}

	composePath := filepath.Join(repoPath, "docker", "docker-compose.yml")
	if err := os.MkdirAll(filepath.Dir(composePath), 0o700); err != nil {
		t.Fatalf("create compose directory: %v", err)
	}

	if err := os.WriteFile(composePath, []byte("services:\n  included:\n    image: busybox\n"), 0o600); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("get worktree: %v", err)
	}

	if _, err := worktree.Add("docker/docker-compose.yml"); err != nil {
		t.Fatalf("stage compose file: %v", err)
	}

	commit, err := worktree.Commit("add compose", &gogit.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit compose file: %v", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), commit)); err != nil {
		t.Fatalf("create main branch: %v", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set main as HEAD: %v", err)
	}

	return repo
}

func installLocalGitTransport(t *testing.T, repo *gogit.Repository) func() {
	t.Helper()
	gitProtocolMu.Lock()

	oldTransport := gitclient.Protocols["git"]

	gitclient.InstallProtocol("git", server.NewClient(repositoryLoader{store: repo.Storer}))

	return func() {
		gitclient.InstallProtocol("git", oldTransport)
		gitProtocolMu.Unlock()
	}
}
