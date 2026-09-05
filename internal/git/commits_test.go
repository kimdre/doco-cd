package git_test

import (
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/git"
)

func TestGetLatestCommit(t *testing.T) {
	t.Parallel()

	c, err := app.GetConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	url := cloneUrl

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

	commit, err := git.GetLatestCommit(repo, git.MainBranch)
	if err != nil {
		t.Fatalf("Failed to get latest commit: %v", err)
	}

	if commit == "" {
		t.Fatal("Commit hash is empty")
	}

	t.Log(commit)
}

func TestGetLatestCommitAnnotatedTag(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}

	if _, err = repo.CreateTag("v1.0.0", head.Hash(), &gogit.CreateTagOptions{
		Tagger: &object.Signature{
			Name:  "test",
			Email: "test@example.com",
			When:  time.Now(),
		},
		Message: "release",
	}); err != nil {
		t.Fatalf("create annotated tag: %v", err)
	}

	commit, err := git.GetLatestCommit(repo, "v1.0.0")
	if err != nil {
		t.Fatalf("GetLatestCommit() error = %v", err)
	}

	if commit != head.Hash().String() {
		t.Fatalf("GetLatestCommit() = %q, want tagged commit %q", commit, head.Hash())
	}
}
