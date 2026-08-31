package git_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const (
	cloneUrl         = "https://github.com/kimdre/doco-cd.git"
	cloneUrlTest     = "https://github.com/kimdre/doco-cd_tests.git"
	cloneUrlSSH      = "git@github.com:kimdre/doco-cd.git"
	remoteMainBranch = "refs/remotes/origin/main"
	remoteTagRef     = "refs/tags/v0.81.0-rc.1"
	tagRef           = "v0.81.0-rc.1"
	invalidRef       = "refs/heads/invalid"
	invalidTagRef    = "refs/tags/invalid"
	commitSHARef     = "bb8864f3fb30cdd36a109f52bc4ab961ec40f5d6"
)

// initLocalTestRepo creates a bare-metal (non-bare) local git repository at path
// with an initial commit on the "main" branch, returning the go-git repository handle.
func initLocalTestRepo(t *testing.T, path string) *gogit.Repository {
	t.Helper()

	repo, err := gogit.PlainInit(path, false)
	if err != nil {
		t.Fatalf("failed to init local test repo: %v", err)
	}

	// Point HEAD at "main" before the first commit so it lands there directly,
	// avoiding a checkout of a not-yet-existing branch on an empty repository.
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("failed to set HEAD to main: %v", err)
	}

	commitLocalTestFile(t, repo, path, "README.md", "initial\n", "initial commit")

	return repo
}

// commitLocalTestFile writes relPath under repoPath, stages and commits it, returning the new commit hash.
func commitLocalTestFile(t *testing.T, repo *gogit.Repository, repoPath, relPath, content, msg string) plumbing.Hash {
	t.Helper()

	filePath := filepath.Join(repoPath, relPath)
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write %s: %v", relPath, err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := wt.Add(relPath); err != nil {
		t.Fatalf("failed to add %s: %v", relPath, err)
	}

	hash, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "local-fs-test",
			Email: "local-fs-test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to commit %q: %v", msg, err)
	}

	return hash
}
