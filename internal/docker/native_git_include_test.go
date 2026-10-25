package docker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/test"
)

func TestGitResourceLoaderNativeGitIncludeIgnoresHTTPToken(t *testing.T) {
	gitProtocolMu.Lock()
	defer gitProtocolMu.Unlock()

	repo := createGitIncludeRepository(t)

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("get Git include fixture worktree: %v", err)
	}

	root := t.TempDir()
	if err := os.Rename(worktree.Filesystem.Root(), filepath.Join(root, "repo.git")); err != nil {
		t.Fatalf("move Git include fixture into daemon root: %v", err)
	}

	baseURL := test.StartGitDaemon(t, root)
	resource := baseURL + "repo.git#main:docker"
	loader := newGitResourceLoader(gitResourceLoaderConfig{
		CacheBase: t.TempDir(),
	})

	if !loader.Accept(resource) {
		t.Fatalf("native Git include resource was rejected: %q", resource)
	}

	loaded, err := loader.Load(context.Background(), resource)
	if err != nil {
		t.Fatalf("load native Git include: %v", err)
	}

	if filepath.Base(loaded) != "docker-compose.yml" || filepath.Base(filepath.Dir(loaded)) != "docker" {
		t.Fatalf("native Git include path = %q, want docker/docker-compose.yml", loaded)
	}

	content, err := os.ReadFile(loaded)

	const want = "services:\n  included:\n    image: busybox\n"

	if err != nil || string(content) != want {
		t.Fatalf("native Git include content = %q, error = %v, want %q", content, err, want)
	}

	if got := loader.Dir(resource); got != filepath.Dir(loaded) {
		t.Fatalf("native Git include directory = %q, want %q", got, filepath.Dir(loaded))
	}

	if got := loader.Dir(loaded); got != filepath.Dir(loaded) {
		t.Fatalf("native Git loaded path directory = %q, want %q", got, filepath.Dir(loaded))
	}
}
