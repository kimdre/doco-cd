package git_test

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/git"
)

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
