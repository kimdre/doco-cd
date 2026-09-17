package git_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/getsops/sops/v3/age"

	"github.com/kimdre/doco-cd/internal/encryption"
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

func TestDecryptedFilesManifest_SecondWriteMergesWithFirst(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	// Two independent stacks in the same repository each decrypt their own
	// files for the same commit; the second write must not drop the first
	// stack's entry.
	if err := git.WriteDecryptedFilesManifest(repoPath, []string{"stack-a/secrets.env"}); err != nil {
		t.Fatalf("WriteDecryptedFilesManifest() error = %v", err)
	}

	if err := git.WriteDecryptedFilesManifest(repoPath, []string{"stack-b/secrets.env"}); err != nil {
		t.Fatalf("WriteDecryptedFilesManifest() error = %v", err)
	}

	recorded := git.ReadDecryptedFilesManifest(repo)

	if !recorded.Contains("stack-a/secrets.env") {
		t.Errorf("second write dropped the first stack's file, got %v", recorded)
	}

	if !recorded.Contains("stack-b/secrets.env") {
		t.Errorf("recorded manifest missing second stack's file: %v", recorded)
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

func TestDecryptedFilesManifest_IsStoredInGitDirNotWorktree(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	if err := git.WriteDecryptedFilesManifest(repoPath, []string{"secrets.env"}); err != nil {
		t.Fatalf("WriteDecryptedFilesManifest() error = %v", err)
	}

	manifestPath := filepath.Join(repoPath, ".git", "doco-cd", "decrypted-manifest.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("expected manifest at %s, stat error = %v", manifestPath, err)
	}

	// The manifest must stay invisible to working-tree scans, so writing it
	// must not make the worktree dirty.
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	status, err := wt.Status()
	if err != nil {
		t.Fatalf("failed to get worktree status: %v", err)
	}

	if !status.IsClean() {
		t.Errorf("expected a clean worktree after writing the manifest, got %v", status)
	}
}

func TestResetTrackedFiles_ReDecryptsFilesRecordedInDecryptedManifest(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	commitLocalTestFile(t, repo, repoPath, "secrets.env", encryptedFixture(t), "add secrets file")

	secretsPath := filepath.Join(repoPath, "secrets.env")
	if err := os.WriteFile(secretsPath, []byte(decryptedFixture(t)), 0o600); err != nil {
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

	// secrets.env must end up decrypted again, never left as the committed ciphertext.
	assertDecrypted(t, secretsPath)

	// README.md is not SOPS content: it must be reset as usual.
	content, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("failed to read README file: %v", err)
	}

	if string(content) != "initial\n" {
		t.Errorf("unrecorded modified tracked file was not reset: got %q, want %q", content, "initial\n")
	}
}

func TestResetTrackedFiles_RedecryptsAfterNewCommit(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	commitLocalTestFile(t, repo, repoPath, "secrets.env", encryptedFixture(t), "add secrets file")

	secretsPath := filepath.Join(repoPath, "secrets.env")
	if err := os.WriteFile(secretsPath, []byte(decryptedFixture(t)), 0o600); err != nil {
		t.Fatalf("failed to simulate in-place decryption: %v", err)
	}

	if err := git.WriteDecryptedFilesManifest(repoPath, []string{secretsPath}); err != nil {
		t.Fatalf("WriteDecryptedFilesManifest() error = %v", err)
	}

	// Advance HEAD so the manifest no longer matches the checked-out commit: the
	// file must still come back decrypted instead of waiting for the stack that
	// owns it to be loaded again.
	commitLocalTestFile(t, repo, repoPath, "other.txt", "x\n", "unrelated commit")

	if err := os.WriteFile(secretsPath, []byte(decryptedFixture(t)), 0o600); err != nil {
		t.Fatalf("failed to re-simulate in-place decryption: %v", err)
	}

	if err := git.ResetTrackedFiles(repo); err != nil {
		t.Fatalf("ResetTrackedFiles() error = %v", err)
	}

	assertDecrypted(t, secretsPath)

	if recorded := git.ReadDecryptedFilesManifest(repo); !recorded.Contains("secrets.env") {
		t.Errorf("manifest was not rewritten for the new commit, got %v", recorded)
	}
}

func TestResetTrackedFiles_RedecryptsWithoutManifest(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	commitLocalTestFile(t, repo, repoPath, "secrets.env", encryptedFixture(t), "add secrets file")

	secretsPath := filepath.Join(repoPath, "secrets.env")
	if err := os.WriteFile(secretsPath, []byte(decryptedFixture(t)), 0o600); err != nil {
		t.Fatalf("failed to simulate in-place decryption: %v", err)
	}

	// No manifest at all (e.g. upgraded from a build that never wrote one):
	// the committed ciphertext is enough to classify the file.
	if err := git.ResetTrackedFiles(repo); err != nil {
		t.Fatalf("ResetTrackedFiles() error = %v", err)
	}

	assertDecrypted(t, secretsPath)
}

func TestResetTrackedFiles_RepairsRecordedFileLeftEncrypted(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	commitLocalTestFile(t, repo, repoPath, "secrets.env", encryptedFixture(t), "add secrets file")

	secretsPath := filepath.Join(repoPath, "secrets.env")

	// The manifest claims the file is decrypted while it is ciphertext on disk:
	// the state an interrupted run leaves behind. Nothing else would ever repair it.
	if err := git.WriteDecryptedFilesManifest(repoPath, []string{secretsPath}); err != nil {
		t.Fatalf("WriteDecryptedFilesManifest() error = %v", err)
	}

	if err := git.ResetTrackedFiles(repo); err != nil {
		t.Fatalf("ResetTrackedFiles() error = %v", err)
	}

	assertDecrypted(t, secretsPath)
}

func TestResetTrackedFiles_DecryptsUpdatedContentOfChangedFile(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	commitLocalTestFile(t, repo, repoPath, "secrets.env", "PLACEHOLDER=1\n", "add placeholder")

	secretsPath := filepath.Join(repoPath, "secrets.env")

	// A commit turns the file into a SOPS document while the worktree copy still
	// holds the old plaintext: the reset must pick up the new ciphertext and
	// decrypt that, not keep the stale worktree content.
	commitLocalTestFile(t, repo, repoPath, "secrets.env", encryptedFixture(t), "encrypt secrets file")

	if err := os.WriteFile(secretsPath, []byte("PLACEHOLDER=1\n"), 0o600); err != nil {
		t.Fatalf("failed to restore stale worktree content: %v", err)
	}

	if err := git.ResetTrackedFiles(repo); err != nil {
		t.Fatalf("ResetTrackedFiles() error = %v", err)
	}

	assertDecrypted(t, secretsPath)
}

func TestResetTrackedFiles_MissingSopsKeyDoesNotFailCheckout(t *testing.T) {
	t.Setenv(age.SopsAgeKeyFileEnv, filepath.Join(t.TempDir(), "missing-key.txt"))
	t.Setenv(age.SopsAgeKeyEnv, "")

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	commitLocalTestFile(t, repo, repoPath, "secrets.env", encryptedFixture(t), "add secrets file")

	secretsPath := filepath.Join(repoPath, "secrets.env")
	if err := os.WriteFile(secretsPath, []byte(decryptedFixture(t)), 0o600); err != nil {
		t.Fatalf("failed to simulate in-place decryption: %v", err)
	}

	// Decryption is best effort here: a repository without a usable SOPS key must
	// still check out, with the failure surfacing when the stack is deployed.
	if err := git.ResetTrackedFiles(repo); err != nil {
		t.Fatalf("ResetTrackedFiles() must not fail when decryption is impossible, got: %v", err)
	}
}

// The submodule checkout in updateSubmodules refuses to run with unstaged
// changes and recovers with a blind hard reset, so the reset that precedes it
// must leave the worktree clean and must not touch the manifest: the record is
// what lets the restore run again once the submodule sits at its new commit.
func TestResetTrackedFilesWithoutRestore_LeavesFileEncryptedAndManifestIntact(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	commitLocalTestFile(t, repo, repoPath, "secrets.env", encryptedFixture(t), "add secrets file")

	secretsPath := filepath.Join(repoPath, "secrets.env")
	if err := os.WriteFile(secretsPath, []byte(decryptedFixture(t)), 0o600); err != nil {
		t.Fatalf("failed to simulate in-place decryption: %v", err)
	}

	if err := git.WriteDecryptedFilesManifest(repoPath, []string{secretsPath}); err != nil {
		t.Fatalf("WriteDecryptedFilesManifest() error = %v", err)
	}

	if err := git.ResetTrackedFilesWithoutRestore(repo); err != nil {
		t.Fatalf("ResetTrackedFilesWithoutRestore() error = %v", err)
	}

	content, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatalf("failed to read secrets file: %v", err)
	}

	if string(content) != encryptedFixture(t) {
		t.Error("expected file to be reset to its committed, encrypted state")
	}

	if recorded := git.ReadDecryptedFilesManifestAny(repo); !recorded.Contains("secrets.env") {
		t.Fatalf("manifest must be left intact, got: %v", recorded.ToSlice())
	}

	// The record still describes reality once the restore runs again.
	if err = git.ResetTrackedFiles(repo); err != nil {
		t.Fatalf("ResetTrackedFiles() error = %v", err)
	}

	assertDecrypted(t, secretsPath)
}

func TestResetTrackedFiles_RetriesFileThatFailedToDecrypt(t *testing.T) {
	t.Setenv(age.SopsAgeKeyFileEnv, filepath.Join(t.TempDir(), "missing-key.txt"))
	t.Setenv(age.SopsAgeKeyEnv, "")

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	commitLocalTestFile(t, repo, repoPath, "secrets.env", encryptedFixture(t), "add secrets file")

	secretsPath := filepath.Join(repoPath, "secrets.env")
	if err := os.WriteFile(secretsPath, []byte(decryptedFixture(t)), 0o600); err != nil {
		t.Fatalf("failed to simulate in-place decryption: %v", err)
	}

	if err := git.ResetTrackedFiles(repo); err != nil {
		t.Fatalf("ResetTrackedFiles() error = %v", err)
	}

	// The file is ciphertext again, so nothing but the manifest can tell it apart
	// from any other tracked file. It has to stay recorded, otherwise a single
	// transient decryption failure disables the repair for good.
	if recorded := git.ReadDecryptedFilesManifestAny(repo); !recorded.Contains("secrets.env") {
		t.Fatalf("file that failed to decrypt was dropped from the manifest, got: %v", recorded.ToSlice())
	}

	// With the key available again, the next reset must repair the file even
	// though it is clean against HEAD.
	encryption.SetupAgeKeyEnvVar(t)

	if err := git.ResetTrackedFiles(repo); err != nil {
		t.Fatalf("ResetTrackedFiles() error = %v", err)
	}

	assertDecrypted(t, secretsPath)
}

func TestResetTrackedFiles_LeavesAlreadyDecryptedFileUntouched(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	commitLocalTestFile(t, repo, repoPath, "secrets.env", encryptedFixture(t), "add secrets file")

	secretsPath := filepath.Join(repoPath, "secrets.env")

	// Bring the file into the state a completed run leaves behind: the exact
	// plaintext of the committed ciphertext.
	plaintext, err := encryption.DecryptFile(secretsPath)
	if err != nil {
		t.Fatalf("failed to decrypt fixture: %v", err)
	}

	if err = os.WriteFile(secretsPath, plaintext, 0o600); err != nil {
		t.Fatalf("failed to simulate in-place decryption: %v", err)
	}

	if err = git.WriteDecryptedFilesManifest(repoPath, []string{secretsPath}); err != nil {
		t.Fatalf("WriteDecryptedFilesManifest() error = %v", err)
	}

	before, err := os.Stat(secretsPath)
	if err != nil {
		t.Fatalf("failed to stat secrets file: %v", err)
	}

	// Make a modification time change detectable on filesystems with coarse timestamps.
	time.Sleep(10 * time.Millisecond)

	if err = git.ResetTrackedFiles(repo); err != nil {
		t.Fatalf("ResetTrackedFiles() error = %v", err)
	}

	after, err := os.Stat(secretsPath)
	if err != nil {
		t.Fatalf("failed to stat secrets file: %v", err)
	}

	// The file must not be rewritten at all: anything watching it (a Traefik
	// dynamic config bind mount, say) would otherwise reload for no reason, and
	// would race a window in which the file holds ciphertext.
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("already decrypted file was rewritten: mod time %s -> %s", before.ModTime(), after.ModTime())
	}

	assertDecrypted(t, secretsPath)
}

func assertDecrypted(t *testing.T, path string) {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}

	if _, encrypted := encryption.DetectFormat(content, path); encrypted {
		t.Errorf("%s was left encrypted after ResetTrackedFiles: %q", path, content)
	}
}

func encryptedFixture(t *testing.T) string {
	t.Helper()

	return readFixture(t, "encrypted.env")
}

func decryptedFixture(t *testing.T) string {
	t.Helper()

	return readFixture(t, "unencrypted.env")
}

func readFixture(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join("..", "encryption", "testdata", name)

	content, err := os.ReadFile(path) // #nosec G304 -- test fixture with a fixed relative path
	if err != nil {
		t.Fatalf("failed to read fixture %s: %v", path, err)
	}

	return string(content)
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
