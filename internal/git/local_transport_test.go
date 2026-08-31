package git_test

import (
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/git"
)

func TestCloneRepository_LocalFileURL(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	dstPath := t.TempDir()

	auth, err := git.GetAuthMethod("file://"+srcPath, "", "", "")
	if err != nil {
		t.Fatalf("GetAuthMethod() error = %v", err)
	}

	if auth != nil {
		t.Fatalf("GetAuthMethod() = %v, want nil for local file URL", auth)
	}

	repo, err := git.CloneRepository(dstPath, "file://"+srcPath, git.MainBranch, false, transport.ProxyOptions{}, auth, false, 0)
	if err != nil {
		t.Fatalf("CloneRepository() error = %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}

	if head.Name().Short() != "main" {
		t.Fatalf("Head() branch = %q, want main", head.Name().Short())
	}
}

func TestFetchRepository_LocalFileURL_DetectsNewCommit(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	srcRepo := initLocalTestRepo(t, srcPath)

	dstPath := t.TempDir()

	repo, err := git.CloneRepository(dstPath, "file://"+srcPath, git.MainBranch, false, transport.ProxyOptions{}, nil, false, 0)
	if err != nil {
		t.Fatalf("CloneRepository() error = %v", err)
	}

	matches, err := git.MatchesHead(dstPath, git.MainBranch)
	if err != nil {
		t.Fatalf("MatchesHead() error = %v", err)
	}

	if !matches {
		t.Fatal("MatchesHead() = false right after clone, want true")
	}

	// Add a new commit to the source repository and verify the clone detects it via fetch.
	newHash := commitLocalTestFile(t, srcRepo, srcPath, "CHANGED.md", "changed\n", "second commit")

	if err := git.FetchRepository(repo, "file://"+srcPath, false, transport.ProxyOptions{}, nil, 0); err != nil {
		t.Fatalf("FetchRepository() error = %v", err)
	}

	matches, err = git.MatchesHead(dstPath, git.MainBranch)
	if err != nil {
		t.Fatalf("MatchesHead() error = %v", err)
	}

	if matches {
		t.Fatal("MatchesHead() = true after fetch but before checkout, want false since the source repo has a new commit")
	}

	if _, err := git.UpdateRepository(dstPath, "file://"+srcPath, git.MainBranch, false, transport.ProxyOptions{}, nil, false, 0); err != nil {
		t.Fatalf("UpdateRepository() error = %v", err)
	}

	matches, err = git.MatchesHead(dstPath, git.MainBranch)
	if err != nil {
		t.Fatalf("MatchesHead() error = %v", err)
	}

	if !matches {
		t.Fatal("MatchesHead() = false after fetch+update, want true")
	}

	updatedRepo, err := git.OpenRepository(dstPath)
	if err != nil {
		t.Fatalf("OpenRepository() error = %v", err)
	}

	head, err := updatedRepo.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}

	if head.Hash() != newHash {
		t.Fatalf("Head() = %s, want %s", head.Hash(), newHash)
	}
}

func TestCloneRepository_LocalFileURL_BareRepository(t *testing.T) {
	t.Parallel()

	// Populate a bare repository by cloning a normal one into it.
	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	barePath := filepath.Join(t.TempDir(), "bare.git")
	if _, err := gogit.PlainClone(barePath, true, &gogit.CloneOptions{URL: "file://" + srcPath}); err != nil {
		t.Fatalf("failed to create bare test repo: %v", err)
	}

	dstPath := t.TempDir()

	repo, err := git.CloneRepository(dstPath, "file://"+barePath, git.MainBranch, false, transport.ProxyOptions{}, nil, false, 0)
	if err != nil {
		t.Fatalf("CloneRepository() from bare repo error = %v", err)
	}

	if _, err = repo.Head(); err != nil {
		t.Fatalf("Head() error = %v", err)
	}
}

func TestCloneRepository_LocalFileURL_GitDirFile(t *testing.T) {
	t.Parallel()

	// Simulate a submodule/worktree layout: the working tree holds a ".git" *file*
	// pointing at the real git directory stored elsewhere.
	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	movedGitDir := filepath.Join(t.TempDir(), "modules", "app")
	if err := os.MkdirAll(filepath.Dir(movedGitDir), 0o750); err != nil {
		t.Fatalf("failed to create git dir parent: %v", err)
	}

	if err := os.Rename(filepath.Join(srcPath, ".git"), movedGitDir); err != nil {
		t.Fatalf("failed to move git dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(srcPath, ".git"), []byte("gitdir: "+movedGitDir+"\n"), 0o600); err != nil {
		t.Fatalf("failed to write .git file: %v", err)
	}

	dstPath := t.TempDir()

	repo, err := git.CloneRepository(dstPath, "file://"+srcPath, git.MainBranch, false, transport.ProxyOptions{}, nil, false, 0)
	if err != nil {
		t.Fatalf("CloneRepository() with .git file error = %v", err)
	}

	if _, err = repo.Head(); err != nil {
		t.Fatalf("Head() error = %v", err)
	}
}

func TestCloneRepository_LocalFileURL_IgnoresShallowDepth(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	srcRepo := initLocalTestRepo(t, srcPath)
	commitLocalTestFile(t, srcRepo, srcPath, "SECOND.md", "second\n", "second commit")
	commitLocalTestFile(t, srcRepo, srcPath, "THIRD.md", "third\n", "third commit")

	dstPath := t.TempDir()

	// The in-process transport does not implement git's shallow capability, so a
	// requested depth must be ignored rather than failing the clone.
	repo, err := git.CloneRepository(dstPath, "file://"+srcPath, git.MainBranch, false, transport.ProxyOptions{}, nil, false, 1)
	if err != nil {
		t.Fatalf("CloneRepository() with depth error = %v", err)
	}

	if _, err = os.Stat(filepath.Join(dstPath, ".git", "shallow")); !os.IsNotExist(err) {
		t.Fatalf("clone of local repository must not be shallow, stat shallow file err = %v", err)
	}

	// A subsequent depth-limited update must not re-clone in a loop or fail.
	if _, err = git.UpdateRepository(dstPath, "file://"+srcPath, git.MainBranch, false, transport.ProxyOptions{}, nil, false, 1); err != nil {
		t.Fatalf("UpdateRepository() with depth error = %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}

	if head.Hash().IsZero() {
		t.Fatal("Head() returned zero hash")
	}
}
