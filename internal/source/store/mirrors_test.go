package store_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/source/store"
)

func mkdirAll(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// makeBareMirror creates the parts of a bare repository ListMirrors looks at.
// An empty remoteURL leaves the configuration without a remote.
func makeBareMirror(t *testing.T, dir, remoteURL string) {
	t.Helper()

	mkdirAll(t, filepath.Join(dir, "objects"))

	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}

	config := "[core]\n\tbare = true\n"
	if remoteURL != "" {
		config += "[remote \"origin\"]\n\turl = " + remoteURL + "\n\tfetch = +refs/*:refs/*\n"
	}

	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestListRepositoryDirs_FindsNestedRepoRoots guards the layout that actually
// exists on disk: git.GetRepoName produces "<host>/<owner>/<repo>", so a
// store base directory is never an immediate child of the data directory.
// Listing only immediate children turned the GC sweeper into a no-op.
func TestListRepositoryDirs_FindsNestedRepoRoots(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()

	gitRepo := filepath.Join(dataDir, "github.com", "owner", "repo-a")
	mkdirAll(t, filepath.Join(gitRepo, store.ArtifactsSubdir, "rev"))
	mkdirAll(t, filepath.Join(gitRepo, store.MirrorSubdir))

	// Deeper nesting, e.g. a GitLab subgroup.
	nested := filepath.Join(dataDir, "gitlab.com", "group", "subgroup", "repo-b")
	mkdirAll(t, filepath.Join(nested, store.ArtifactsSubdir, "rev"))

	// Not a store base directory, and must not be reported.
	mkdirAll(t, filepath.Join(dataDir, "unrelated", "stuff"))

	if err := os.WriteFile(filepath.Join(dataDir, "stray-file.txt"), []byte(""), 0o600); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	dirs, err := store.ListRepositoryDirs(dataDir)
	if err != nil {
		t.Fatalf("ListRepositoryDirs() error = %v", err)
	}

	slices.Sort(dirs)

	want := []string{gitRepo, nested}
	if !slices.Equal(dirs, want) {
		t.Fatalf("ListRepositoryDirs() = %v, want %v", dirs, want)
	}

	dirs, err = store.ListRepositoryDirs(filepath.Join(dataDir, "does-not-exist"))
	if err != nil {
		t.Fatalf("ListRepositoryDirs(missing) error = %v, want nil", err)
	}

	if len(dirs) != 0 {
		t.Fatalf("ListRepositoryDirs(missing) = %v, want empty", dirs)
	}
}

// TestListRepositoryDirs_DoesNotDescendIntoArtifacts makes sure a published
// artifact that happens to contain an "artifacts" or "mirror" directory of its
// own is never mistaken for a store base directory.
func TestListRepositoryDirs_DoesNotDescendIntoArtifacts(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()

	repoDir := filepath.Join(dataDir, "github.com", "owner", "repo")
	artifact := filepath.Join(repoDir, store.ArtifactsSubdir, "rev")
	mkdirAll(t, filepath.Join(artifact, store.MirrorSubdir))

	dirs, err := store.ListRepositoryDirs(dataDir)
	if err != nil {
		t.Fatalf("ListRepositoryDirs() error = %v", err)
	}

	if len(dirs) != 1 || dirs[0] != repoDir {
		t.Fatalf("ListRepositoryDirs() = %v, want exactly [%q]", dirs, repoDir)
	}
}

func TestListMirrors(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()

	// A Git store whose revisions use two submodules.
	repoA := filepath.Join(dataDir, "github.com", "owner", "repo-a")
	makeBareMirror(t, filepath.Join(repoA, store.MirrorSubdir), "ssh://git@github.com/owner/repo-a")
	makeBareMirror(t, filepath.Join(repoA, store.SubmodulesSubdir, "aaa"), "ssh://git@github.com/owner/lib")
	makeBareMirror(t, filepath.Join(repoA, store.SubmodulesSubdir, "bbb"), "https://gitlab.com/group/sub/tool.git")
	// A submodule mirror still being cloned has no remote yet.
	makeBareMirror(t, filepath.Join(repoA, store.SubmodulesSubdir, "ccc"), "")
	// Lock files live next to the mirrors.
	if err := os.WriteFile(filepath.Join(repoA, store.SubmodulesSubdir, "aaa.lock"), nil, 0o600); err != nil {
		t.Fatalf("write lock file: %v", err)
	}

	// The submodule's own repository is deployed as well.
	lib := filepath.Join(dataDir, "github.com", "owner", "lib")
	makeBareMirror(t, filepath.Join(lib, store.MirrorSubdir), "ssh://git@github.com/owner/lib")

	// An OCI store has artifacts but no mirror.
	mkdirAll(t, filepath.Join(dataDir, "ghcr.io", "owner", "image", store.ArtifactsSubdir, "rev"))

	// A mirror directory that is not a repository.
	mkdirAll(t, filepath.Join(dataDir, "github.com", "owner", "broken", store.MirrorSubdir))

	mirrors, err := store.ListMirrors(dataDir)
	if err != nil {
		t.Fatalf("ListMirrors() error = %v", err)
	}

	want := []store.Mirror{
		{Repository: "github.com/owner/lib", Path: filepath.Join(lib, store.MirrorSubdir)},
		{Repository: "github.com/owner/lib", Path: filepath.Join(repoA, store.SubmodulesSubdir, "aaa")},
		{Repository: "github.com/owner/repo-a", Path: filepath.Join(repoA, store.MirrorSubdir)},
		{Repository: "gitlab.com/group/sub/tool", Path: filepath.Join(repoA, store.SubmodulesSubdir, "bbb")},
	}
	if !slices.Equal(mirrors, want) {
		t.Fatalf("ListMirrors() =\n%v\nwant\n%v", mirrors, want)
	}

	mirrors, err = store.ListMirrors(filepath.Join(dataDir, "does-not-exist"))
	if err != nil || len(mirrors) != 0 {
		t.Fatalf("ListMirrors(missing) = %v, %v, want none", mirrors, err)
	}
}

func TestListMirrors_ComposeIncludes(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	repoDir := filepath.Join(dataDir, "github.com", "owner", "app")
	makeBareMirror(t, filepath.Join(repoDir, store.MirrorSubdir), "https://github.com/owner/app.git")

	// Compose puts its include cache next to the published revision, not at
	// the data directory's top level. The store walk stops before this cache.
	include := filepath.Join(repoDir, store.ArtifactsSubdir, store.ComposeGitCacheSubdir, "aaa")
	makeBareMirror(t, filepath.Join(include, store.MirrorSubdir), "https://github.com/owner/lib.git")
	makeBareMirror(t, filepath.Join(include, store.SubmodulesSubdir, "bbb"), "https://github.com/owner/tool.git")

	// Older/fallback include caches can also live directly below dataDir.
	fallbackInclude := filepath.Join(dataDir, store.ComposeGitCacheSubdir, "ccc")
	makeBareMirror(t, filepath.Join(fallbackInclude, store.MirrorSubdir), "https://github.com/owner/lib.git")

	// Neither arbitrary artifact contents nor incomplete clones are mirrors.
	makeBareMirror(t, filepath.Join(repoDir, store.ArtifactsSubdir, "rev", "mirror"), "https://github.com/owner/not-a-cache.git")
	makeBareMirror(t, filepath.Join(repoDir, store.ArtifactsSubdir, store.ComposeGitCacheSubdir, "ddd", store.MirrorSubdir), "")

	mirrors, err := store.ListMirrors(dataDir)
	if err != nil {
		t.Fatalf("ListMirrors() error = %v", err)
	}

	want := []store.Mirror{
		{Repository: "github.com/owner/app", Path: filepath.Join(repoDir, store.MirrorSubdir)},
		{Repository: "github.com/owner/lib", Path: filepath.Join(fallbackInclude, store.MirrorSubdir)},
		{Repository: "github.com/owner/lib", Path: filepath.Join(include, store.MirrorSubdir)},
		{Repository: "github.com/owner/tool", Path: filepath.Join(include, store.SubmodulesSubdir, "bbb")},
	}
	if !slices.Equal(mirrors, want) {
		t.Fatalf("ListMirrors() =\n%v\nwant\n%v", mirrors, want)
	}
}

func TestListMirrors_InvalidConfiguration(t *testing.T) {
	t.Parallel()

	for _, subdir := range []string{store.MirrorSubdir, filepath.Join(store.SubmodulesSubdir, "aaa")} {
		t.Run(subdir, func(t *testing.T) {
			t.Parallel()

			dataDir := t.TempDir()
			repoDir := filepath.Join(dataDir, "github.com", "owner", "app")
			mkdirAll(t, filepath.Join(repoDir, store.ArtifactsSubdir))

			mirrorDir := filepath.Join(repoDir, subdir)
			makeBareMirror(t, mirrorDir, "")

			configPath := filepath.Join(mirrorDir, "config")
			if err := os.WriteFile(configPath, []byte("[invalid\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := store.ListMirrors(dataDir); err == nil || !strings.Contains(err.Error(), "parse mirror configuration") {
				t.Fatalf("ListMirrors() error = %v, want a configuration error", err)
			}

			// A clone that has not written its configuration yet is omitted.
			if err := os.Remove(configPath); err != nil {
				t.Fatal(err)
			}

			if mirrors, err := store.ListMirrors(dataDir); err != nil || len(mirrors) != 0 {
				t.Fatalf("ListMirrors(incomplete clone) = %v, %v, want none", mirrors, err)
			}
		})
	}
}
