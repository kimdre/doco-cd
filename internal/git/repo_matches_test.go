package git_test

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/git"
)

func TestMatchesHead(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		reference string
	}{
		{"matches branch", "test"},
		{"matches branch ref", "refs/heads/test"},
		{"matches tag", "v1.0.0"},
		{"matches tag ref", "refs/tags/v1.0.0"},
		{"matches commit SHA", "a6e74091c5bb5913c0daff4d3fc8c1d1b2ad826b"},
	}

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	auth, err := git.GetAuthMethod(cloneUrlTest, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	dir := t.TempDir()

	repo, err := git.CloneRepository(dir, cloneUrlTest, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("repository is nil after clone")
	}

	matched, err := git.MatchesHead(dir, git.MainBranch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !matched {
		t.Fatalf("expected repo to match reference after clone")
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err = git.CheckoutRepository(repo, tc.reference, auth, c.GitCloneSubmodules)
			if err != nil {
				t.Fatalf("failed to checkout reference '%s': %v", tc.reference, err)
			}

			matched, err = git.MatchesHead(dir, tc.reference)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !matched {
				// Get current head for debugging
				headRef, err := repo.Head()
				if err != nil {
					t.Fatalf("%s: %v", git.ErrGetHeadFailed.Error(), err)
				}

				t.Errorf("expected repo to match reference '%s' but got '%s'", tc.reference, headRef.Name().String())
			}
		})
	}
}

// This tests the MatchesHead function's ability to detect a mismatch after a checkout to a different branch.
func TestMatchesHead_AfterCheckoutToDifferentBranch(t *testing.T) {
	t.Parallel()

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	auth, err := git.GetAuthMethod(cloneUrlTest, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	// Clone repository to temp dir
	dir := t.TempDir()

	repo, err := git.CloneRepository(dir, cloneUrlTest, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("repository is nil after clone")
	}

	// Check against a different branch (should not match but repo should be returned)
	matched, err := git.MatchesHead(dir, git.MainBranch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !matched {
		t.Fatalf("expected repo to match reference after clone")
	}

	err = git.CheckoutRepository(repo, "test", auth, c.GitCloneSubmodules)
	if err != nil {
		t.Fatalf("failed to checkout test branch: %v", err)
	}

	// Check again after checkout: the repository is now on 'test', so asking if it matches 'main' should return false
	matched, err = git.MatchesHead(dir, git.MainBranch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if matched {
		t.Errorf("expected repo to not match reference after checkout to different branch")

		// Get the current branch to confirm it's 'test'
		headRef, err := repo.Head()
		if err != nil {
			t.Fatalf("%s: %v", git.ErrGetHeadFailed.Error(), err)
		}

		if headRef.Name().Short() != "test" {
			t.Fatalf("expected current branch to be 'test', got '%s'", headRef.Name().Short())
		}
	}
}
