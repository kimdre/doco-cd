package git_test

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/git"
)

func TestRepoMatches_MatchingRemoteAndRef(t *testing.T) {
	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	auth, err := git.GetAuthMethod(cloneUrl, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	// Clone repository to temp dir
	dir := t.TempDir()

	repo, err := git.CloneRepository(dir, cloneUrl, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("repository is nil after clone")
	}

	matched, err := git.RepoMatches(dir, cloneUrl, git.MainBranch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !matched {
		t.Fatalf("expected repo to match remote+ref")
	}
}

func TestRepoMatches_MismatchedRemote(t *testing.T) {
	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	auth, err := git.GetAuthMethod(cloneUrl, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	// Clone repository to temp dir
	dir := t.TempDir()

	repo, err := git.CloneRepository(dir, cloneUrl, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("repository is nil after clone")
	}

	// Check against a different URL (should not match but repo should be returned)
	matched, err := git.RepoMatches(dir, cloneUrlTest, git.MainBranch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if matched {
		t.Fatalf("expected repo to not match when remote URL differs")
	}
}

func TestRepoMatches_MatchingCommitSHA(t *testing.T) {
	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	auth, err := git.GetAuthMethod(cloneUrl, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	// Clone repository to temp dir
	dir := t.TempDir()

	repo, err := git.CloneRepository(dir, cloneUrl, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("repository is nil after clone")
	}

	// Use a known commit SHA from existing tests
	matched, err := git.RepoMatches(dir, cloneUrl, commitSHARef)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !matched {
		t.Fatalf("expected repo to match when commit SHA exists locally")
	}
}
