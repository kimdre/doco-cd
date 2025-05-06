package git

import (
	"fmt"
	"os"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/google/uuid"
	"github.com/kimdre/doco-cd/internal/config"
)

const (
	cloneUrl         = "https://github.com/kimdre/doco-cd.git"
	validRef         = "refs/heads/main"
	invalidRef       = "refs/heads/invalid"
	validCommitSHA   = "903b270da7505fe8b13b42d3b191b08fb9ca3247"
	invalidCommitSHA = "1111111111111111111111111111111111111111"
	testDir          = "/tmp/doco-cd-tests/"
)

func TestGetAuthUrl(t *testing.T) {
	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	expectedUrl := fmt.Sprintf("https://%s:%s@github.com/kimdre/doco-cd.git", c.AuthType, c.GitAccessToken)

	authUrl := GetAuthUrl(
		"https://github.com/kimdre/doco-cd.git",
		c.AuthType,
		c.GitAccessToken,
	)

	if authUrl != expectedUrl {
		t.Fatalf("Expected %s, got %s", expectedUrl, authUrl)
	}
}

func TestCloneRepository(t *testing.T) {
	repo, err := CloneRepository(testDir+uuid.New().String(), cloneUrl, validRef, false)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil")
	}

	// Check files in the repository
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	t.Cleanup(func() {
		err = os.RemoveAll(worktree.Filesystem.Root())
		if err != nil {
			t.Fatalf("Failed to remove repository: %v", err)
		}
	})

	files, err := worktree.Filesystem.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed to read directory: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("No files in repository")
	}

	// Check if the repository is cloned
	if worktree.Filesystem.Root() == "" {
		t.Fatal("Repository is not cloned")
	}
}

// TestCheckoutRepository tests the CheckoutRepository function on an already cloned repository
func TestCheckoutRepository(t *testing.T) {
	repo, err := CloneRepository(testDir+uuid.New().String(), cloneUrl, validRef, false)
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

	t.Cleanup(func() {
		err = os.RemoveAll(worktree.Filesystem.Root())
		if err != nil {
			t.Fatalf("Failed to remove repository: %v", err)
		}
	})

	repo, err = CheckoutRepository(worktree.Filesystem.Root(), validRef, validCommitSHA, true)
	if err != nil {
		t.Fatalf("Failed to checkout repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil")
	}

	// Check if the commit exists
	_, err = repo.CommitObject(plumbing.NewHash(validCommitSHA))
	if err != nil {
		t.Fatalf("Failed to get commit object: %v", err)
	}

	// Check if the commit is checked out
	worktree, err = repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	if worktree.Filesystem.Root() == "" {
		t.Fatal("Repository is not checked out")
	}
}
