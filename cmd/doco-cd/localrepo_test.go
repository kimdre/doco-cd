package main

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

// mergeFiles combines one or more file maps (relative path -> content) into
// a single map, for composing fixture file sets.
func mergeFiles(fileMaps ...map[string]string) map[string]string {
	merged := make(map[string]string)

	for _, files := range fileMaps {
		maps.Copy(merged, files)
	}

	return merged
}

// newLocalFixtureRepo creates a small ephemeral git repository on
// refs/heads/main with the given files committed, and returns its file://
// clone URL and commit hash. It backs TestHandleEvent sub-tests that deploy
// a repository, so tests run fully offline.
func newLocalFixtureRepo(t *testing.T, files map[string]string) (repo *gogit.Repository, cloneURL string, hash plumbing.Hash) {
	t.Helper()

	repoPath := t.TempDir()

	repo, err := gogit.PlainInitWithOptions(repoPath, &gogit.PlainInitOptions{
		DefaultBranch: plumbing.NewBranchReferenceName("main"),
	})
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	for relPath, content := range files {
		fullPath := filepath.Join(repoPath, relPath)

		if err = os.MkdirAll(filepath.Dir(fullPath), filesystem.PermDir); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(fullPath), err)
		}

		if err = os.WriteFile(fullPath, []byte(content), filesystem.PermOwner); err != nil {
			t.Fatalf("write %s: %v", relPath, err)
		}

		if _, err = wt.Add(relPath); err != nil {
			t.Fatalf("git add %s: %v", relPath, err)
		}
	}

	hash, err = wt.Commit("initial commit", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "doco-cd-test",
			Email: "doco-cd-test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	return repo, "file://" + repoPath, hash
}
