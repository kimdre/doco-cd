package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// readEncryptionFixture returns the content of a fixture file from
// internal/encryption/testdata, so this package's tests exercise the exact
// same SOPS-encrypted content the encryption package's own tests use,
// without duplicating it inline.
func readEncryptionFixture(t *testing.T, name string) string {
	t.Helper()

	_, filename, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(filename), "..", "..", "encryption", "testdata", name)

	content, err := os.ReadFile(path) // #nosec G304 -- test fixture path built from a fixed name, not user input
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	return string(content)
}

// TestGitStore_PublishDecryptsArtifact commits a SOPS-encrypted file and
// asserts Publish's artifact directory contains its decrypted plaintext.
func TestGitStore_PublishDecryptsArtifact(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)
	commitTestFile(t, repo, repoPath, "secret.yaml", readEncryptionFixture(t, "encrypted.yaml"), "add encrypted file")

	s := newGitStore(t, "file://"+repoPath)

	rev, err := s.Resolve(context.Background(), "main")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	artifact, err := s.Publish(context.Background(), rev)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(artifact.Path, "secret.yaml"))
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}

	const want = "this.is.encrypted: \"yes\"\n"
	if string(got) != want {
		t.Fatalf("published file = %q, want %q (decrypted plaintext)", got, want)
	}
}

// TestGitStore_PublishKeepsUndecryptableFileAsCiphertext verifies a file the
// instance holds no key for does not fail the publish - which would take down
// every stack in the repository, including those that never read it. It stays
// ciphertext, and a stack that does consume it fails later, at load time.
func TestGitStore_PublishKeepsUndecryptableFileAsCiphertext(t *testing.T) {
	fixture := readEncryptionFixture(t, "encrypted.yaml")

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)
	commitTestFile(t, repo, repoPath, "secret.yaml", fixture, "add encrypted file")
	commitTestFile(t, repo, repoPath, "compose.yaml", "services: {}\n", "add compose file")

	s := newGitStore(t, "file://"+repoPath)

	rev, err := s.Resolve(context.Background(), "main")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	artifact, err := s.Publish(context.Background(), rev)
	if err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}

	got, err := os.ReadFile(filepath.Join(artifact.Path, "secret.yaml"))
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}

	if string(got) != fixture {
		t.Fatalf("published file = %q, want the original ciphertext", got)
	}

	if _, err := os.ReadFile(filepath.Join(artifact.Path, "compose.yaml")); err != nil {
		t.Fatalf("read unrelated published file: %v", err)
	}
}

// TestOCIStore_PublishDecryptsArtifact mirrors
// TestGitStore_PublishDecryptsArtifact for an OCI-backed artifact.
func TestOCIStore_PublishDecryptsArtifact(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	ref := newTestRegistryRef(t, "latest")
	digest := pushTestArtifact(t, ref, map[string]string{
		".doco-cd.yaml": "name: test\n",
		"secret.yaml":   readEncryptionFixture(t, "encrypted.yaml"),
	})

	s, err := store.NewOCIStore(store.OCIStoreOptions{ArtifactRef: ref.String(), BaseDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewOCIStore() error = %v", err)
	}

	artifact, err := s.Publish(context.Background(), store.Revision(digest))
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(artifact.Path, "secret.yaml"))
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}

	const want = "this.is.encrypted: \"yes\"\n"
	if string(got) != want {
		t.Fatalf("published file = %q, want %q (decrypted plaintext)", got, want)
	}
}
