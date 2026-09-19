package store_test

import (
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/source/store"
)

func TestArtifactRoot_RecoversRootAndRevisionFromDescendantPath(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	descendant := filepath.Join(baseDir, "artifacts", "abc123", "sub", "dir")

	root, revision, ok := store.ArtifactRoot(baseDir, descendant)
	if !ok {
		t.Fatalf("ArtifactRoot() ok = false, want true")
	}

	wantRoot := filepath.Join(baseDir, "artifacts", "abc123")
	if root != wantRoot {
		t.Errorf("root = %q, want %q", root, wantRoot)
	}

	if revision != "abc123" {
		t.Errorf("revision = %q, want %q", revision, "abc123")
	}
}

func TestArtifactRoot_RecoversFromArtifactRootItself(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	artifactRoot := filepath.Join(baseDir, "artifacts", "abc123")

	root, revision, ok := store.ArtifactRoot(baseDir, artifactRoot)
	if !ok {
		t.Fatalf("ArtifactRoot() ok = false, want true")
	}

	if root != artifactRoot {
		t.Errorf("root = %q, want %q", root, artifactRoot)
	}

	if revision != "abc123" {
		t.Errorf("revision = %q, want %q", revision, "abc123")
	}
}

func TestArtifactRoot_RejectsPathsOutsideArtifactsDir(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()

	cases := map[string]string{
		"unrelated path":            filepath.Join(baseDir, "mirror"),
		"artifacts dir itself":      filepath.Join(baseDir, "artifacts"),
		"escaping via traversal":    filepath.Join(baseDir, "artifacts", "..", "mirror"),
		"a completely unrelated fs": "/etc/passwd",
	}

	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, _, ok := store.ArtifactRoot(baseDir, path); ok {
				t.Errorf("ArtifactRoot(%q) ok = true, want false", path)
			}
		})
	}
}
