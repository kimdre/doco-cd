package git_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/git"
)

func TestDecryptedFilesManifest_RoundTrip(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	if err := git.WriteDecryptedFilesManifest(repoPath, []string{
		filepath.Join(repoPath, "secrets.env"),
		"nested/config.yaml",
	}); err != nil {
		t.Fatalf("WriteDecryptedFilesManifest() error = %v", err)
	}

	recorded := git.ReadDecryptedFilesManifest(repo)

	if !recorded.Contains("secrets.env") {
		t.Errorf("recorded manifest missing absolute-path file normalized to repo-relative: %v", recorded)
	}

	if !recorded.Contains("nested/config.yaml") {
		t.Errorf("recorded manifest missing repo-relative file: %v", recorded)
	}

	if recorded.Len() != 2 {
		t.Errorf("recorded manifest length = %d, want 2 (%v)", recorded.Len(), recorded)
	}
}

func TestDecryptedFilesManifest_StaleAfterNewCommit(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	if err := git.WriteDecryptedFilesManifest(repoPath, []string{"README.md"}); err != nil {
		t.Fatalf("WriteDecryptedFilesManifest() error = %v", err)
	}

	if recorded := git.ReadDecryptedFilesManifest(repo); !recorded.Contains("README.md") {
		t.Fatalf("expected manifest to be readable before the commit changes, got %v", recorded)
	}

	// Move HEAD to a new commit without updating the manifest: it must now be
	// treated as stale (empty), since it was recorded for the previous commit.
	commitLocalTestFile(t, repo, repoPath, "README.md", "changed\n", "second commit")

	if recorded := git.ReadDecryptedFilesManifest(repo); !recorded.IsEmpty() {
		t.Errorf("manifest recorded for a stale commit should be ignored, got %v", recorded)
	}
}

func TestDecryptedFilesManifest_MissingManifestReturnsEmptySet(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	recorded := git.ReadDecryptedFilesManifest(repo)
	if !recorded.IsEmpty() {
		t.Errorf("expected an empty set when no manifest was ever written, got %v", recorded)
	}
}

func TestDecryptedFilesManifest_NonGitRepoRootIsANoOp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := git.WriteDecryptedFilesManifest(dir, []string{"a.env"}); err != nil {
		t.Fatalf("WriteDecryptedFilesManifest() on a non-Git directory should be a no-op, got error: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read temp dir: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("expected no files to be written for a non-Git directory, found: %v", entries)
	}
}

func TestResetTrackedFiles_SkipsFilesRecordedInDecryptedManifest(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	commitLocalTestFile(t, repo, repoPath, "secrets.env", "encrypted-content\n", "add secrets file")

	secretsPath := filepath.Join(repoPath, "secrets.env")
	if err := os.WriteFile(secretsPath, []byte("decrypted-content\n"), 0o600); err != nil {
		t.Fatalf("failed to simulate in-place decryption: %v", err)
	}

	readmePath := filepath.Join(repoPath, "README.md")
	if err := os.WriteFile(readmePath, []byte("modified\n"), 0o600); err != nil {
		t.Fatalf("failed to modify tracked file: %v", err)
	}

	if err := git.WriteDecryptedFilesManifest(repoPath, []string{secretsPath}); err != nil {
		t.Fatalf("WriteDecryptedFilesManifest() error = %v", err)
	}

	if err := git.ResetTrackedFiles(repo); err != nil {
		t.Fatalf("ResetTrackedFiles() error = %v", err)
	}

	// secrets.env is recorded in the manifest for the current commit: it must
	// be left alone (still decrypted), not reset to its committed ciphertext.
	content, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatalf("failed to read secrets file: %v", err)
	}

	if string(content) != "decrypted-content\n" {
		t.Errorf("manifest-recorded file was reset: got %q, want %q", content, "decrypted-content\n")
	}

	// README.md was not recorded in the manifest: it must be reset as usual.
	content, err = os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("failed to read README file: %v", err)
	}

	if string(content) != "initial\n" {
		t.Errorf("unrecorded modified tracked file was not reset: got %q, want %q", content, "initial\n")
	}
}

func TestResetTrackedFiles_ManifestForDifferentCommitDoesNotProtectFile(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	commitLocalTestFile(t, repo, repoPath, "secrets.env", "encrypted-v1\n", "add secrets file")

	secretsPath := filepath.Join(repoPath, "secrets.env")

	// Record a manifest for the current commit, then advance HEAD: the
	// manifest is now stale and must not protect secrets.env any more, even
	// though its path is still listed.
	if err := git.WriteDecryptedFilesManifest(repoPath, []string{secretsPath}); err != nil {
		t.Fatalf("WriteDecryptedFilesManifest() error = %v", err)
	}

	commitLocalTestFile(t, repo, repoPath, "secrets.env", "encrypted-v2\n", "rotate secrets file")

	if err := os.WriteFile(secretsPath, []byte("locally-modified\n"), 0o600); err != nil {
		t.Fatalf("failed to modify tracked file: %v", err)
	}

	if err := git.ResetTrackedFiles(repo); err != nil {
		t.Fatalf("ResetTrackedFiles() error = %v", err)
	}

	content, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatalf("failed to read secrets file: %v", err)
	}

	if string(content) != "encrypted-v2\n" {
		t.Errorf("file protected by a stale manifest was not reset: got %q, want %q", content, "encrypted-v2\n")
	}
}
