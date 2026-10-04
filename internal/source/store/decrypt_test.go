package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/getsops/sops/v3/age"

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
	// No key configured, so the encrypted file below cannot be decrypted.
	// CI also injects SOPS_AGE_KEY - the same recipient this fixture is
	// encrypted for - as an ambient env var for the whole test binary, so it
	// must be cleared explicitly to make this test deterministic there too.
	t.Setenv(age.SopsAgeKeyEnv, "")
	t.Setenv(age.SopsAgeKeyFileEnv, "")

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

// publishRevision resolves and publishes ref with s, failing the test on error.
func publishRevision(t *testing.T, s store.Store, ref string) store.Artifact {
	t.Helper()

	rev, err := s.Resolve(t.Context(), ref)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	artifact, err := s.Publish(t.Context(), rev)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	return artifact
}

// readArtifactFile returns the content of rel inside artifact.
func readArtifactFile(t *testing.T, artifact store.Artifact, rel string) string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(artifact.Path, rel)) // #nosec G304 -- test path
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}

	return string(content)
}

// TestGitStore_PublishReusesPlaintextOfUnchangedCiphertext publishes a second
// revision without any SOPS key and expects the unchanged secret decrypted,
// which proves the plaintext came from the first artifact and not from a key service.
func TestGitStore_PublishReusesPlaintextOfUnchangedCiphertext(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	const want = "this.is.encrypted: \"yes\"\n"

	fixture := readEncryptionFixture(t, "encrypted.yaml")

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)
	commitTestFile(t, repo, repoPath, "secret.yaml", fixture, "add encrypted file")

	baseDir := t.TempDir()

	newStore := func() *store.GitStore {
		s, err := store.NewGitStore(store.GitStoreOptions{CloneURL: "file://" + repoPath, BaseDir: baseDir})
		if err != nil {
			t.Fatalf("NewGitStore() error = %v", err)
		}

		return s
	}

	first := publishRevision(t, newStore(), "main")

	if got := readArtifactFile(t, first, "secret.yaml"); got != want {
		t.Fatalf("first artifact = %q, want %q", got, want)
	}

	if _, err := os.Stat(first.Path + ".decrypted.json"); err != nil {
		t.Fatalf("decrypt record: %v", err)
	}

	commitTestFile(t, repo, repoPath, "compose.yaml", "services: {}\n", "unrelated change")
	t.Setenv(age.SopsAgeKeyEnv, "")
	t.Setenv(age.SopsAgeKeyFileEnv, "")

	second := publishRevision(t, newStore(), "main")

	if got := readArtifactFile(t, second, "secret.yaml"); got != want {
		t.Fatalf("second artifact = %q, want reused plaintext %q", got, want)
	}

	// A plaintext that changed after it was recorded must not be reused,
	// even with the original size and timestamps.
	tampered := filepath.Join(second.Path, "secret.yaml")

	info, err := os.Stat(tampered)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	forged := []byte(strings.Repeat("x", int(info.Size())))
	if err = os.WriteFile(tampered, forged, 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	if err = os.Chtimes(tampered, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	if err := os.Remove(first.Path + ".decrypted.json"); err != nil {
		t.Fatalf("remove first record: %v", err)
	}

	commitTestFile(t, repo, repoPath, "compose.yaml", "services: {} # v2\n", "another unrelated change")

	third := publishRevision(t, newStore(), "main")

	if got := readArtifactFile(t, third, "secret.yaml"); got != fixture {
		t.Fatalf("third artifact = %q, want ciphertext (no key, no trusted plaintext)", got)
	}
}

// TestGitStore_PublishDoesNotReusePlaintextAcrossFormats renames identical
// ciphertext from a binary file to a JSON file. The format comes from the
// extension, so the plaintext differs and must not be reused.
func TestGitStore_PublishDoesNotReusePlaintextAcrossFormats(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	fixture := readEncryptionFixture(t, "encrypted")

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)
	commitTestFile(t, repo, repoPath, "secret", fixture, "add binary secret")

	baseDir := t.TempDir()

	newStore := func() *store.GitStore {
		s, err := store.NewGitStore(store.GitStoreOptions{CloneURL: "file://" + repoPath, BaseDir: baseDir})
		if err != nil {
			t.Fatalf("NewGitStore() error = %v", err)
		}

		return s
	}

	first := publishRevision(t, newStore(), "main")
	binary := readArtifactFile(t, first, "secret")

	commitTestFile(t, repo, repoPath, "secret.json", fixture, "same ciphertext as json")

	second := publishRevision(t, newStore(), "main")
	asJSON := readArtifactFile(t, second, "secret.json")

	if asJSON == binary || asJSON == fixture {
		t.Fatalf("secret.json = %q, want a fresh JSON decryption, binary plaintext is %q", asJSON, binary)
	}

	if !strings.HasPrefix(asJSON, "{") {
		t.Fatalf("secret.json = %q, want JSON plaintext", asJSON)
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
