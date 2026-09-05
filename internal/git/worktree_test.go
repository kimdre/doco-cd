package git_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/git"
)

func TestResetTrackedFiles_ResetsTrackedFilesAndKeepsUntrackedFiles(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	readmePath := filepath.Join(repoPath, "README.md")
	if err := os.WriteFile(readmePath, []byte("modified\n"), 0o600); err != nil {
		t.Fatalf("failed to modify tracked file: %v", err)
	}

	untrackedPath := filepath.Join(repoPath, "untracked.txt")
	if err := os.WriteFile(untrackedPath, []byte("untracked\n"), 0o600); err != nil {
		t.Fatalf("failed to create untracked file: %v", err)
	}

	if err := git.ResetTrackedFiles(repo); err != nil {
		t.Fatalf("failed to reset tracked files: %v", err)
	}

	content, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("failed to read reset tracked file: %v", err)
	}
	if string(content) != "initial\n" {
		t.Errorf("tracked file = %q, want %q", content, "initial\n")
	}

	if _, err := os.Stat(untrackedPath); err != nil {
		t.Errorf("untracked file was removed: %v", err)
	}
}

func TestUpdateRepository_KeepUntrackedFiles(t *testing.T) {
	t.Parallel()

	c, err := app.GetConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	url := cloneUrlTest

	auth, err := git.GetAuthMethod(url, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	if auth != nil {
		t.Logf("Using auth method: %s", auth.Name())
	} else {
		t.Log("No auth method configured, using anonymous access")
	}

	repo, err := git.CloneRepository(t.TempDir(), url, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules, 0)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil")
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	// Add a new file to the cloned repository
	newFileName := "new.txt"

	_, err = worktree.Filesystem.Create(newFileName)
	if err != nil {
		t.Fatalf("Failed to create new file: %v", err)
	}

	repo, err = git.UpdateRepository(worktree.Filesystem.Root(), url, "alternative", false, c.HttpProxy, auth, c.GitCloneSubmodules, 0)
	if err != nil {
		t.Fatalf("Failed to update repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil after update")
	}

	files, err := worktree.Filesystem.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed to read directory: %v", err)
	}

	foundNewFile := false

	for _, file := range files {
		if file.Name() == newFileName {
			foundNewFile = true
			break
		}
	}

	if !foundNewFile {
		t.Fatal("Untracked file was removed during update")
	}
}
