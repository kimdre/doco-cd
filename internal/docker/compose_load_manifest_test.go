package docker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	gogitplumbing "github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/encryption"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/test"
)

// initGitRepoWithFiles creates a Git repository at repoPath containing the given
// relative-path -> content files, committed to the "main" branch, and returns the
// opened repository.
func initGitRepoWithFiles(t *testing.T, repoPath string, files map[string][]byte) *git.Repository {
	t.Helper()

	repo, err := git.PlainInit(repoPath, false)
	if err != nil {
		t.Fatalf("failed to init test repo: %v", err)
	}

	if err := repo.Storer.SetReference(gogitplumbing.NewSymbolicReference(gogitplumbing.HEAD, gogitplumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("failed to set HEAD to main: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	for relPath, content := range files {
		fullPath := filepath.Join(repoPath, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
			t.Fatalf("failed to create directory for %s: %v", relPath, err)
		}

		if err := os.WriteFile(fullPath, content, 0o600); err != nil {
			t.Fatalf("failed to write %s: %v", relPath, err)
		}

		if _, err := worktree.Add(relPath); err != nil {
			t.Fatalf("failed to stage %s: %v", relPath, err)
		}
	}

	if _, err := worktree.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "docker-test",
			Email: "docker-test@example.com",
			When:  time.Now(),
		},
	}); err != nil {
		t.Fatalf("failed to commit files: %v", err)
	}

	return repo
}

// TestLoadCompose_PersistsDecryptedFilesManifest proves that LoadCompose records every
// file it decrypts in place into the Git-directory-local decrypted-files manifest
// (internal/git.WriteDecryptedFilesManifest), tagged with the commit currently checked
// out, so that a later checkout (internal/git.ResetTrackedFiles) can tell the decrypted
// working copy apart from stale/reverted content without re-running SOPS.
func TestLoadCompose_PersistsDecryptedFilesManifest(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	encryptedEnv, err := os.ReadFile("../encryption/testdata/encrypted.env")
	if err != nil {
		t.Fatalf("failed to read encrypted fixture: %v", err)
	}

	repoPath := t.TempDir()

	const (
		composeRelPath = "docker-compose.yaml"
		envRelPath     = ".env"
	)

	repo := initGitRepoWithFiles(t, repoPath, map[string][]byte{
		composeRelPath: []byte(generateComposeContents()),
		envRelPath:     encryptedEnv,
	})

	composeFilePath := filepath.Join(repoPath, composeRelPath)
	envFilePath := filepath.Join(repoPath, envRelPath)

	_, err = LoadCompose(context.Background(), nil, repoPath, repoPath, test.ConvertTestName(t.Name()),
		[]string{composeFilePath}, []string{envRelPath}, []string{}, map[string]string{}, ComposeLoadOptions{})
	if err != nil {
		t.Fatalf("LoadCompose() error = %v", err)
	}

	manifest := gitInternal.ReadDecryptedFilesManifest(repo)
	if !manifest.Contains(envRelPath) {
		t.Fatalf("decrypted-files manifest = %v, want it to contain %q", manifest, envRelPath)
	}

	// A subsequent reset must leave the now-decrypted env file alone instead
	// of reverting it to its committed, still-encrypted form.
	if err := gitInternal.ResetTrackedFiles(repo); err != nil {
		t.Fatalf("ResetTrackedFiles() error = %v", err)
	}

	afterReset, err := os.ReadFile(envFilePath)
	if err != nil {
		t.Fatalf("failed to read env file after reset: %v", err)
	}

	if string(afterReset) == string(encryptedEnv) {
		t.Error("decrypted env file was reset back to its encrypted committed content")
	}
}
