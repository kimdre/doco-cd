package git_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/git"
)

// commitLocalTestExecutable writes an executable file at relPath under
// repoPath, stages and commits it.
func commitLocalTestExecutable(t *testing.T, repo *gogit.Repository, repoPath, relPath, content, msg string) {
	t.Helper()

	filePath := filepath.Join(repoPath, relPath)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("failed to create parent directory for %s: %v", relPath, err)
	}

	if err := os.WriteFile(filePath, []byte(content), 0o755); err != nil { //nolint:gosec // test fixture needs the executable bit set to verify it round-trips
		t.Fatalf("failed to write %s: %v", relPath, err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := wt.Add(relPath); err != nil {
		t.Fatalf("failed to add %s: %v", relPath, err)
	}

	if _, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: "local-fs-test", Email: "local-fs-test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("failed to commit %q: %v", msg, err)
	}
}

// commitLocalTestSymlink creates a symlink at relPath pointing at target,
// stages and commits it.
func commitLocalTestSymlink(t *testing.T, repo *gogit.Repository, repoPath, relPath, target, msg string) {
	t.Helper()

	linkPath := filepath.Join(repoPath, relPath)
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatalf("failed to create parent directory for %s: %v", relPath, err)
	}

	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("failed to create symlink %s: %v", relPath, err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := wt.Add(relPath); err != nil {
		t.Fatalf("failed to add %s: %v", relPath, err)
	}

	if _, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: "local-fs-test", Email: "local-fs-test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("failed to commit %q: %v", msg, err)
	}
}

func TestExportTree_FilesDirsAndExecutableBit(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	repo := initLocalTestRepo(t, srcPath)

	if err := os.MkdirAll(filepath.Join(srcPath, "nested", "dir"), 0o755); err != nil {
		t.Fatalf("failed to create nested directory: %v", err)
	}

	commitLocalTestFile(t, repo, srcPath, "nested/dir/config.yaml", "key: value\n", "add nested file")
	commitLocalTestExecutable(t, repo, srcPath, "bin/run.sh", "#!/bin/sh\necho hi\n", "add executable")

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}

	dir := t.TempDir()
	if err := git.ExportTree(dir, repo, head.Hash(), git.ExportOptions{}); err != nil {
		t.Fatalf("ExportTree() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dir, "nested", "dir", "config.yaml"))
	if err != nil {
		t.Fatalf("read exported nested file: %v", err)
	}

	if string(content) != "key: value\n" {
		t.Fatalf("nested file content = %q, want %q", content, "key: value\n")
	}

	info, err := os.Stat(filepath.Join(dir, "bin", "run.sh"))
	if err != nil {
		t.Fatalf("stat exported executable: %v", err)
	}

	if info.Mode()&0o111 == 0 {
		t.Fatalf("exported executable mode = %v, want executable bit set", info.Mode())
	}

	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("read exported README.md: %v", err)
	}

	if info, err := os.Stat(filepath.Join(dir, "README.md")); err != nil || info.Mode()&0o111 != 0 {
		t.Fatalf("README.md mode = %v (err=%v), want no executable bit", info, err)
	}

	if string(readme) != "initial\n" {
		t.Fatalf("README.md content = %q, want %q", readme, "initial\n")
	}
}

func TestExportTree_Symlink(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	repo := initLocalTestRepo(t, srcPath)

	commitLocalTestFile(t, repo, srcPath, "target.txt", "hello\n", "add target")
	commitLocalTestSymlink(t, repo, srcPath, "link.txt", "target.txt", "add symlink")

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}

	dir := t.TempDir()
	if err := git.ExportTree(dir, repo, head.Hash(), git.ExportOptions{}); err != nil {
		t.Fatalf("ExportTree() error = %v", err)
	}

	linkPath := filepath.Join(dir, "link.txt")

	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("lstat exported symlink: %v", err)
	}

	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("exported link.txt is not a symlink, mode = %v", info.Mode())
	}

	resolvedTarget, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}

	if resolvedTarget != "target.txt" {
		t.Fatalf("symlink target = %q, want %q", resolvedTarget, "target.txt")
	}

	content, err := os.ReadFile(linkPath)
	if err != nil {
		t.Fatalf("read via symlink: %v", err)
	}

	if string(content) != "hello\n" {
		t.Fatalf("symlink content = %q, want %q", content, "hello\n")
	}
}

func TestExportTree_RejectsSymlinkEscapingRootViaTraversal(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	repo := initLocalTestRepo(t, srcPath)

	escapingTarget := strings.Repeat("../", 20) + "etc/passwd"
	commitLocalTestSymlink(t, repo, srcPath, "escape.txt", escapingTarget, "add traversal symlink")

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}

	dir := t.TempDir()

	err = git.ExportTree(dir, repo, head.Hash(), git.ExportOptions{})
	if err == nil {
		t.Fatal("ExportTree() error = nil, want a path traversal error")
	}

	if _, statErr := os.Lstat(filepath.Join(dir, "escape.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("expected escape.txt to not be created, stat err = %v", statErr)
	}
}

func TestExportTree_RejectsAbsoluteSymlinkTarget(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	repo := initLocalTestRepo(t, srcPath)

	commitLocalTestSymlink(t, repo, srcPath, "escape.txt", "/etc/passwd", "add absolute symlink")

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}

	dir := t.TempDir()

	err = git.ExportTree(dir, repo, head.Hash(), git.ExportOptions{})
	if err == nil {
		t.Fatal("ExportTree() error = nil, want a path traversal error for an absolute symlink target")
	}

	if _, statErr := os.Lstat(filepath.Join(dir, "escape.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("expected escape.txt to not be created, stat err = %v", statErr)
	}
}
