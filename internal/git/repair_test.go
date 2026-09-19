package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/filesystem/dotgit"

	"github.com/kimdre/doco-cd/internal/filesystem"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

func TestIsCorruptionError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "reference not found", err: ErrInvalidReference, want: true},
		{name: "object not found", err: plumbing.ErrObjectNotFound, want: true},
		{name: "empty ref file", err: fmt.Errorf("fetch failed: %w", dotgit.ErrEmptyRefFile), want: true},
		{name: "malformed packed refs", err: dotgit.ErrPackedRefsBadFormat, want: true},
		{name: "duplicated packed ref", err: dotgit.ErrPackedRefsDuplicatedRef, want: true},
		{name: "symbolic ref target not found", err: dotgit.ErrSymRefTargetNotFound, want: true},
		{name: "wrapped message fallback", err: errors.New("fetch failed: reference not found"), want: true},
		{name: "other error", err: ErrMissingAuthToken, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCorruptionError(tt.err); got != tt.want {
				t.Fatalf("IsCorruptionError(%v)=%v want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestFetchRepository_UsesPathLock(t *testing.T) {
	t.Parallel()

	originPath, clonePath, _ := setupLocalMainRepoAndClone(t)

	repo, err := gogit.PlainOpen(clonePath)
	if err != nil {
		t.Fatalf("open clone: %v", err)
	}

	unlock := sourcecache.AcquirePathLock(clonePath)

	done := make(chan error, 1)
	go func() {
		done <- FetchRepository(repo, originPath, false, transport.ProxyOptions{}, nil, 0)
	}()

	select {
	case err := <-done:
		unlock()
		t.Fatalf("fetch completed while path lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("fetch failed after path lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fetch did not complete after path lock release")
	}
}

// setupLocalMainRepoAndClone creates a local origin repository with two
// commits on "main" and a checked-out clone of it, returning both paths and
// the origin's main commit hash. Shared by tests that need ordinary fetch/
// reference plumbing against a working-tree clone (not a bare mirror).
func setupLocalMainRepoAndClone(t *testing.T) (originPath string, clonePath string, originMain plumbing.Hash) {
	t.Helper()

	base := t.TempDir()
	originPath = filepath.Join(base, "origin")
	clonePath = filepath.Join(base, "clone")

	originRepo, err := gogit.PlainInit(originPath, false)
	if err != nil {
		t.Fatalf("init origin: %v", err)
	}

	firstHash := commitFile(t, originRepo, originPath, "README.md", "hello\n", "initial commit")

	if err := originRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), firstHash)); err != nil {
		t.Fatalf("set main ref: %v", err)
	}

	if err := originRepo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set HEAD->main: %v", err)
	}

	_ = originRepo.Storer.RemoveReference(plumbing.NewBranchReferenceName("master"))

	originMain = commitFile(t, originRepo, originPath, "README.md", "hello from main\n", "main update")

	clonedRepo, err := gogit.PlainClone(clonePath, false, &gogit.CloneOptions{
		URL:           originPath,
		RemoteName:    RemoteName,
		ReferenceName: plumbing.NewBranchReferenceName("main"),
	})
	if err != nil {
		t.Fatalf("clone repo: %v", err)
	}

	latest, err := GetLatestCommit(clonedRepo, MainBranch)
	if err != nil {
		t.Fatalf("GetLatestCommit(%s): %v", MainBranch, err)
	}

	if latest != originMain.String() {
		t.Fatalf("latest commit mismatch: got %s want %s", latest, originMain)
	}

	return originPath, clonePath, originMain
}

func commitFile(t *testing.T, repo *gogit.Repository, repoPath, relPath, content, msg string) plumbing.Hash {
	t.Helper()

	filePath := filepath.Join(repoPath, relPath)
	if err := os.MkdirAll(filepath.Dir(filePath), filesystem.PermDir); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(filePath), err)
	}

	if err := os.WriteFile(filePath, []byte(content), filesystem.PermOwner); err != nil {
		t.Fatalf("write %s: %v", relPath, err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	if _, err := wt.Add(relPath); err != nil {
		t.Fatalf("git add %s: %v", relPath, err)
	}

	hash, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "repair-test",
			Email: "repair-test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("commit %q: %v", msg, err)
	}

	return hash
}

func BenchmarkIsCorruptionError(b *testing.B) {
	err := errors.New("reference not found")

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = IsCorruptionError(err)
	}
}
