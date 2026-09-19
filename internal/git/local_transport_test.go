package git_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/git"
)

// TestCloneOrUpdateBareMirror_LocalFileURL_GitDirFile exercises the in-process
// file:// transport against a source repository whose ".git" is a file
// pointing elsewhere (the layout Git uses for submodules and worktrees),
// verifying the mirror fetch still resolves it correctly.
func TestCloneOrUpdateBareMirror_LocalFileURL_GitDirFile(t *testing.T) {
	t.Parallel()

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

	mirrorPath := filepath.Join(t.TempDir(), "mirror")

	repo, err := git.CloneOrUpdateBareMirror(nil, "file://"+srcPath, git.MainBranch, mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 0)
	if err != nil {
		t.Fatalf("CloneOrUpdateBareMirror() with .git file error = %v", err)
	}

	if _, err = repo.Head(); err != nil {
		t.Fatalf("Head() error = %v", err)
	}
}
