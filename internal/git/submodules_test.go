package git_test

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/git"
)

func TestCloneRepository_WithSubmodule(t *testing.T) {
	t.Parallel()

	c, err := app.GetConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	auth, err := git.GetAuthMethod(cloneUrlTest, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	if auth != nil {
		t.Logf("Using auth method: %s", auth.Name())
	} else {
		t.Log("No auth method configured, using anonymous access")
	}

	repo, err := git.CloneRepository(t.TempDir(), cloneUrlTest, "with-submodule", false, c.HttpProxy, auth, true, 0)
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

	submodules, err := worktree.Submodules()
	if err != nil {
		t.Fatalf("Failed to get submodules: %v", err)
	}

	if len(submodules) == 0 {
		t.Fatal("No submodules found, but expected one")
	}

	submodule := submodules[0]

	if submodule.Config().Path != "doco-cd_tests" {
		t.Fatalf("Expected submodule path 'doco-cd_tests', got '%s'", submodule.Config().Path)
	}

	// Check if submodule is initialized by reading the README.md file in the submodule directory
	subRepo, err := submodule.Repository()
	if err != nil {
		t.Fatalf("Failed to get submodule repository: %v", err)
	}

	subWorktree, err := subRepo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get submodule worktree: %v", err)
	}

	files, err := subWorktree.Filesystem.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed to read submodule directory: %v", err)
	}

	foundReadme := false

	for _, file := range files {
		if file.Name() == "README.md" {
			foundReadme = true
			break
		}
	}

	if !foundReadme {
		t.Fatal("Submodule is not initialized, README.md not found")
	}
}

func TestUpdateRepository_WithSubmodule(t *testing.T) {
	t.Parallel()

	c, err := app.GetConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	auth, err := git.GetAuthMethod(cloneUrlTest, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	if auth != nil {
		t.Logf("Using auth method: %s", auth.Name())
	} else {
		t.Log("No auth method configured, using anonymous access")
	}

	repo, err := git.CloneRepository(t.TempDir(), cloneUrlTest, git.MainBranch, false, c.HttpProxy, auth, true, 0)
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

	// Check if any submodules exist before update
	submodules, err := worktree.Submodules()
	if err != nil {
		t.Fatalf("Failed to get submodules: %v", err)
	}

	if len(submodules) != 0 {
		t.Fatal("Expected no submodules before update, but found some")
	}

	repo, err = git.UpdateRepository(worktree.Filesystem.Root(), cloneUrlTest, "with-submodule", false, c.HttpProxy, auth, true, 0)
	if err != nil {
		t.Fatalf("Failed to update repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil")
	}

	worktree, err = repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	submodules, err = worktree.Submodules()
	if err != nil {
		t.Fatalf("Failed to get submodules: %v", err)
	}

	if len(submodules) == 0 {
		t.Fatal("No submodules found, but expected one")
	}

	submodule := submodules[0]

	if submodule.Config().Path != "doco-cd_tests" {
		t.Fatalf("Expected submodule path 'doco-cd_tests', got '%s'", submodule.Config().Path)
	}

	// Check if submodule is initialized by reading the README.md file in the submodule directory
	subRepo, err := submodule.Repository()
	if err != nil {
		t.Fatalf("Failed to get submodule repository: %v", err)
	}

	subWorktree, err := subRepo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get submodule worktree: %v", err)
	}

	files, err := subWorktree.Filesystem.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed to read submodule directory: %v", err)
	}

	foundReadme := false

	for _, file := range files {
		if file.Name() == "README.md" {
			foundReadme = true
			break
		}
	}

	if !foundReadme {
		t.Fatal("Submodule is not initialized, README.md not found")
	}
}
