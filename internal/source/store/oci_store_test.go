package store_test

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/kimdre/doco-cd/internal/source/store"
)

// pushTestArtifact builds a minimal doco-cd v1 layout artifact (a single
// ".doco-cd.yaml" file) and pushes it to ref, returning its digest.
func pushTestArtifact(t *testing.T, ref name.Reference, files map[string]string) string {
	t.Helper()

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	for fileName, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: fileName, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatalf("write tar header for %s: %v", fileName, err)
		}

		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write tar content for %s: %v", fileName, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}

	tarBytes := buf.Bytes()

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(tarBytes)), nil
	})
	if err != nil {
		t.Fatalf("build layer: %v", err)
	}

	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("append layer: %v", err)
	}

	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push test artifact: %v", err)
	}

	digest, err := img.Digest()
	if err != nil {
		t.Fatalf("compute digest: %v", err)
	}

	return digest.String()
}

// newTestRegistryRef starts an in-process OCI registry and returns a parsed
// reference for repo:tag on it, along with a cleanup-registered server.
func newTestRegistryRef(t *testing.T, tag string) name.Reference {
	t.Helper()

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")

	ref, err := name.ParseReference(fmt.Sprintf("%s/repo:%s", host, tag), name.WeakValidation)
	if err != nil {
		t.Fatalf("parse test reference: %v", err)
	}

	return ref
}

func TestNewOCIStore_RequiresArtifactRefAndBaseDir(t *testing.T) {
	t.Parallel()

	if _, err := store.NewOCIStore(store.OCIStoreOptions{BaseDir: t.TempDir()}); err == nil {
		t.Fatal("expected an error for missing ArtifactRef")
	}

	if _, err := store.NewOCIStore(store.OCIStoreOptions{ArtifactRef: "example.com/repo:latest"}); err == nil {
		t.Fatal("expected an error for missing BaseDir")
	}
}

func TestOCIStore_ResolveThenPublish(t *testing.T) {
	t.Parallel()

	ref := newTestRegistryRef(t, "latest")
	digest := pushTestArtifact(t, ref, map[string]string{".doco-cd.yaml": "version: doco.v1\n"})

	s, err := store.NewOCIStore(store.OCIStoreOptions{
		ArtifactRef: ref.Name(),
		BaseDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewOCIStore() error = %v", err)
	}

	revision, err := s.Resolve(t.Context(), "")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if string(revision) != digest {
		t.Fatalf("Resolve() = %q, want %q", revision, digest)
	}

	artifact, err := s.Publish(t.Context(), revision)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(artifact.Path, ".doco-cd.yaml"))
	if err != nil {
		t.Fatalf("read extracted .doco-cd.yaml: %v", err)
	}

	if string(content) != "version: doco.v1\n" {
		t.Fatalf(".doco-cd.yaml content = %q, want %q", content, "version: doco.v1\n")
	}
}

func TestOCIStore_PublishUsesResolvedDigestAfterTagMoves(t *testing.T) {
	t.Parallel()

	ref := newTestRegistryRef(t, "latest")
	firstDigest := pushTestArtifact(t, ref, map[string]string{
		".doco-cd.yaml": "version: doco.v1\n",
		"marker.txt":    "first\n",
	})

	s, err := store.NewOCIStore(store.OCIStoreOptions{
		ArtifactRef: ref.Name(),
		BaseDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewOCIStore() error = %v", err)
	}

	pushTestArtifact(t, ref, map[string]string{
		".doco-cd.yaml": "version: doco.v1\n",
		"marker.txt":    "second\n",
	})

	artifact, err := s.Publish(t.Context(), store.Revision(firstDigest))
	if err != nil {
		t.Fatalf("Publish() error after tag moved = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(artifact.Path, "marker.txt"))
	if err != nil {
		t.Fatalf("read extracted config: %v", err)
	}

	if string(content) != "first\n" {
		t.Fatalf("published moved-tag content = %q, want original digest content", content)
	}
}

func TestOCIStore_ResolveRejectsDigestMismatch(t *testing.T) {
	t.Parallel()

	ref := newTestRegistryRef(t, "latest")
	pushTestArtifact(t, ref, map[string]string{".doco-cd.yaml": "version: doco.v1\n"})

	s, err := store.NewOCIStore(store.OCIStoreOptions{
		ArtifactRef: ref.Name(),
		BaseDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewOCIStore() error = %v", err)
	}

	wrongDigest := "sha256:" + strings.Repeat("0", 64)

	if _, err := s.Resolve(t.Context(), wrongDigest); err == nil {
		t.Fatal("expected an error for a mismatched expected digest")
	}
}

func TestOCIStore_PublishIsIdempotentAndDoesNotRepull(t *testing.T) {
	t.Parallel()

	ref := newTestRegistryRef(t, "latest")
	digest := pushTestArtifact(t, ref, map[string]string{".doco-cd.yaml": "version: doco.v1\n"})

	baseDir := t.TempDir()

	s, err := store.NewOCIStore(store.OCIStoreOptions{
		ArtifactRef: ref.Name(),
		BaseDir:     baseDir,
	})
	if err != nil {
		t.Fatalf("NewOCIStore() error = %v", err)
	}

	first, err := s.Publish(t.Context(), store.Revision(digest))
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}

	// Publish again with a store pointed at an artifact reference that no
	// longer resolves - if Publish redid the pull instead of reusing the
	// looked-up artifact, this would fail.
	broken, err := store.NewOCIStore(store.OCIStoreOptions{
		ArtifactRef: strings.Replace(ref.Name(), "repo", "does-not-exist", 1),
		BaseDir:     baseDir,
	})
	if err != nil {
		t.Fatalf("NewOCIStore() error = %v", err)
	}

	second, err := broken.Publish(t.Context(), store.Revision(digest))
	if err != nil {
		t.Fatalf("second Publish() error = %v", err)
	}

	if second.Path != first.Path {
		t.Fatalf("Publish() paths differ across calls: %q vs %q", second.Path, first.Path)
	}
}

func TestOCIStore_LookupAndList(t *testing.T) {
	t.Parallel()

	ref := newTestRegistryRef(t, "latest")
	digest := pushTestArtifact(t, ref, map[string]string{".doco-cd.yaml": "version: doco.v1\n"})

	s, err := store.NewOCIStore(store.OCIStoreOptions{
		ArtifactRef: ref.Name(),
		BaseDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewOCIStore() error = %v", err)
	}

	revision := store.Revision(digest)

	if _, ok, err := s.Lookup(revision); err != nil || ok {
		t.Fatalf("Lookup() before Publish = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	published, err := s.Publish(t.Context(), revision)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	looked, ok, err := s.Lookup(revision)
	if err != nil || !ok {
		t.Fatalf("Lookup() after Publish = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if looked.Path != published.Path {
		t.Fatalf("Lookup() path = %q, want %q", looked.Path, published.Path)
	}

	all, err := s.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(all) != 1 || all[0].Revision != revision {
		t.Fatalf("List() = %+v, want a single entry for %q", all, revision)
	}
}
