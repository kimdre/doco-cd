package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// cloneUrlTestWithSubmodule is the same real, network-hosted fixture
// internal/git's TestCloneRepository_WithSubmodule uses: its
// "with-submodule" branch adds itself as a submodule (relative URL
// "../doco-cd_tests.git") at path "doco-cd_tests". It also contains a
// SOPS-encrypted "test.enc.env" file (encrypted to the same test age key as
// internal/encryption/testdata), which Publish's decrypt-at-publish step
// needs a configured key for - hence these tests cannot run in parallel
// with t.Setenv.
const cloneUrlTestWithSubmodule = "https://github.com/kimdre/doco-cd_tests.git"

func TestGitStore_PublishMaterializesSubmodule(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	s, err := store.NewGitStore(store.GitStoreOptions{
		CloneURL:        cloneUrlTestWithSubmodule,
		BaseDir:         t.TempDir(),
		CloneSubmodules: true,
	})
	if err != nil {
		t.Fatalf("NewGitStore() error = %v", err)
	}

	revision, err := s.Resolve(t.Context(), "refs/heads/with-submodule")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	artifact, err := s.Publish(t.Context(), revision)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	readme, err := os.ReadFile(filepath.Join(artifact.Path, "doco-cd_tests", "README.md"))
	if err != nil {
		t.Fatalf("read submodule README.md: %v", err)
	}

	if len(readme) == 0 {
		t.Fatal("submodule README.md is empty")
	}

	// The fixture's own top-level "test.enc.env" is SOPS-encrypted; Publish
	// must decrypt it in place, the same as any other file in the tree.
	env, err := os.ReadFile(filepath.Join(artifact.Path, "test.enc.env"))
	if err != nil {
		t.Fatalf("read decrypted test.enc.env: %v", err)
	}

	if strings.Contains(string(env), "ENC[") {
		t.Fatalf("test.enc.env was not decrypted, still contains SOPS ciphertext: %s", env)
	}
}

func TestGitStore_PublishLeavesSubmoduleEmptyWhenDisabled(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	s, err := store.NewGitStore(store.GitStoreOptions{
		CloneURL:        cloneUrlTestWithSubmodule,
		BaseDir:         t.TempDir(),
		CloneSubmodules: false,
	})
	if err != nil {
		t.Fatalf("NewGitStore() error = %v", err)
	}

	revision, err := s.Resolve(t.Context(), "refs/heads/with-submodule")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	artifact, err := s.Publish(t.Context(), revision)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(artifact.Path, "doco-cd_tests"))
	if err != nil {
		t.Fatalf("read submodule directory: %v", err)
	}

	if len(entries) != 0 {
		t.Fatalf("expected an empty submodule directory (submodules disabled), got %d entries", len(entries))
	}
}
