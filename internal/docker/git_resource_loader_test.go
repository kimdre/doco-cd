package docker

import (
	"context"
	"errors"
	"fmt"
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

func TestGitResourceLoaderCacheBaseDefaultsToTempDir(t *testing.T) {
	l := newGitResourceLoader(gitResourceLoaderConfig{})
	if l.cacheDirectory != filepath.Join(os.TempDir(), gitIncludeCacheDirectory) {
		t.Fatalf("expected cache inside os.TempDir(), got %q", l.cacheDirectory)
	}
}

func TestGitResourceLoaderDirFallbackPaths(t *testing.T) {
	l := newGitResourceLoader(gitResourceLoaderConfig{CacheBase: t.TempDir()})

	// Unknown resource that does not exist on disk → empty string.
	if got := l.Dir("/nonexistent/path/that/does/not/exist"); got != "" {
		t.Fatalf("expected empty string for unknown/nonexistent resource, got %q", got)
	}

	// Known file on disk → its directory.
	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yaml")

	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatalf("create file: %v", err)
	}

	if got := l.Dir(file); got != dir {
		t.Fatalf("expected dir %q for file resource, got %q", dir, got)
	}

	// Known directory on disk → the directory itself.
	if got := l.Dir(dir); got != dir {
		t.Fatalf("expected dir %q for directory resource, got %q", dir, got)
	}
}

func TestGitResourceLoaderContextCancellation(t *testing.T) {
	l := newGitResourceLoader(gitResourceLoaderConfig{CacheBase: t.TempDir()})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := l.Load(ctx, "https://github.com/example/repo.git")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestGitResourceLoaderLoadsExplicitComposeFile(t *testing.T) {
	repo := createGitIncludeRepository(t)

	restoreTransport := installLocalGitTransport(t, repo)
	defer restoreTransport()

	cacheBase := t.TempDir()
	// Point directly at the file, not the directory.
	resource := "git://example.test/repo.git#main:docker/docker-compose.yml"
	l := newGitResourceLoader(gitResourceLoaderConfig{CacheBase: cacheBase})

	loaded, err := l.Load(context.Background(), resource)
	if err != nil {
		t.Fatalf("load git include: %v", err)
	}

	if filepath.Base(loaded) != "docker-compose.yml" {
		t.Fatalf("expected docker-compose.yml, got %q", loaded)
	}

	// Dir must still return the file's parent directory.
	if got := l.Dir(resource); got != filepath.Dir(loaded) {
		t.Fatalf("Dir(resource)=%q want %q", got, filepath.Dir(loaded))
	}

	if got := l.Dir(loaded); got != filepath.Dir(loaded) {
		t.Fatalf("Dir(localPath)=%q want %q", got, filepath.Dir(loaded))
	}
}

func TestGitResourceLoaderUpdatesCachedRepository(t *testing.T) {
	repo := createGitIncludeRepository(t)

	restoreTransport := installLocalGitTransport(t, repo)
	defer restoreTransport()

	cacheBase := t.TempDir()
	resource := "git://example.test/repo.git#main:docker"
	l := newGitResourceLoader(gitResourceLoaderConfig{CacheBase: cacheBase})

	// First load clones.
	first, err := l.Load(context.Background(), resource)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}

	// Second load must hit the update path (repo already exists) and produce the same file.
	second, err := l.Load(context.Background(), resource)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	if first != second {
		t.Fatalf("expected same path on re-load, got %q vs %q", first, second)
	}
}

func TestGitResourceLoaderConcurrentLoads(t *testing.T) {
	repo := createGitIncludeRepository(t)

	restoreTransport := installLocalGitTransport(t, repo)
	defer restoreTransport()

	cacheBase := t.TempDir()
	resource := "git://example.test/repo.git#main:docker"
	l := newGitResourceLoader(gitResourceLoaderConfig{CacheBase: cacheBase})

	const goroutines = 4

	results := make([]string, goroutines)
	errs := make([]error, goroutines)

	var wg sync.WaitGroup

	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			results[i], errs[i] = l.Load(context.Background(), resource)
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}

		if results[i] == "" {
			t.Errorf("goroutine %d: empty result", i)
		}
	}
}

func TestFindComposeFileReturnsErrorWhenNoneFound(t *testing.T) {
	empty := t.TempDir()
	if _, err := findComposeFile(empty); err == nil {
		t.Fatal("expected error for directory with no compose file")
	}
}

func TestFindComposeFilePrefersFirstDefaultName(t *testing.T) {
	dir := t.TempDir()

	// Write all default names and ensure the first one wins.
	for i, name := range []string{"docker-compose.yaml", "docker-compose.yml", "compose.yml", "compose.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), fmt.Appendf(nil, "# %d", i), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	found, err := findComposeFile(dir)
	if err != nil {
		t.Fatalf("findComposeFile: %v", err)
	}

	// The first entry in cli.DefaultFileNames is "compose.yaml".
	if filepath.Base(found) != "compose.yaml" {
		t.Fatalf("expected compose.yaml (first default name), got %q", filepath.Base(found))
	}
}

// gitIncludeIntegrationEnvVar enables real network tests for Compose Git includes.
const gitIncludeIntegrationEnvVar = "DOCO_CD_RUN_GIT_INCLUDE_INTEGRATION_TESTS"

func skipUnlessGitIncludeIntegration(t *testing.T) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping Git include integration tests in short mode")
	}

	if os.Getenv(gitIncludeIntegrationEnvVar) != "1" {
		t.Skipf("set %s=1 to run Git include integration tests", gitIncludeIntegrationEnvVar)
	}
}

func TestLoadComposeWithRealPublicGitInclude(t *testing.T) {
	skipUnlessGitIncludeIntegration(t)

	// dev.compose.yaml in this repository defines a compose service "doco-cd",
	// so it is a stable, self-contained real-world target.
	const resource = "https://github.com/kimdre/doco-cd.git#main:dev.compose.yaml"

	l := newGitResourceLoader(gitResourceLoaderConfig{CacheBase: t.TempDir()})

	if !l.Accept(resource) {
		t.Fatalf("expected loader to accept %q", resource)
	}

	loaded, err := l.Load(context.Background(), resource)
	if err != nil {
		t.Fatalf("load real Git include: %v", err)
	}

	if _, err := os.Stat(loaded); err != nil {
		t.Fatalf("loaded path does not exist: %v", err)
	}

	if got := l.Dir(resource); got == "" {
		t.Fatal("Dir(resource) returned empty string")
	}
}

func TestLoadComposeWithRealPublicGitIncludeFullRoundtrip(t *testing.T) {
	skipUnlessGitIncludeIntegration(t)

	workingDirectory := t.TempDir()
	t.Setenv("DATA_MOUNT_PATH", t.TempDir())
	t.Setenv("WEBHOOK_SECRET", "test-secret")

	// docker-compose.yml at the root of this repo defines the "doco-cd" service
	// and has no .env or extends dependencies, making it a safe integration target.
	const includedResource = "https://github.com/kimdre/doco-cd.git#main:docker-compose.yml"

	rootCompose := "include:\n  - path: " + includedResource + "\n"
	rootComposePath := filepath.Join(workingDirectory, "compose.yaml")

	if err := os.WriteFile(rootComposePath, []byte(rootCompose), 0o600); err != nil {
		t.Fatalf("write root compose file: %v", err)
	}

	project, err := LoadCompose(
		context.Background(),
		nil,
		workingDirectory,
		workingDirectory,
		"git-include-integration",
		[]string{rootComposePath},
		nil,
		nil,
		map[string]string{},
	)
	if err != nil {
		t.Fatalf("LoadCompose with real Git include: %v", err)
	}

	// docker-compose.yml in this repo defines the "app" service (container_name: doco-cd).
	if _, err := project.GetService("app"); err != nil {
		t.Fatalf("expected 'app' service from included file: %v", err)
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
