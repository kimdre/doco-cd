package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/encryption"
)

// ResetTrackedFiles resets all tracked files in the worktree To their last committed state
// while leaving untracked files intact.
func ResetTrackedFiles(repo *git.Repository) error {
	worktree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	repoRoot := worktree.Filesystem.Root()

	changedFiles, err := worktree.Status()
	if err != nil {
		return fmt.Errorf("failed to get worktree status: %w", err)
	}

	resetFiles := make([]string, 0, len(changedFiles))

	headRef, err := repo.Head()
	if err != nil {
		return resetChangedFiles(worktree, changedFiles)
	}

	commit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return resetChangedFiles(worktree, changedFiles)
	}

	tree, err := commit.Tree()
	if err != nil {
		return resetChangedFiles(worktree, changedFiles)
	}

	for file, status := range changedFiles {
		// Do not touch files that are not part of the Git repository (e.g. created by a container process)
		if status.Staging == git.Untracked {
			continue
		}

		if shouldResetDecryptedFile(tree, repoRoot, file) {
			resetFiles = append(resetFiles, file)
		}
	}

	return resetFilesInWorktree(worktree, resetFiles)
}

// resetChangedFiles resets all changed files in the worktree to their last committed state.
func resetChangedFiles(worktree *git.Worktree, changedFiles git.Status) error {
	resetFiles := make([]string, 0, len(changedFiles))

	for file, status := range changedFiles {
		if status.Staging != git.Untracked {
			resetFiles = append(resetFiles, file)
		}
	}

	return resetFilesInWorktree(worktree, resetFiles)
}

// resetFilesInWorktree resets the specified files in the worktree to their last committed state.
func resetFilesInWorktree(worktree *git.Worktree, files []string) error {
	if len(files) == 0 {
		return nil
	}

	if err := worktree.Reset(&git.ResetOptions{
		Mode:  git.HardReset,
		Files: files,
	}); err != nil {
		return fmt.Errorf("failed to reset worktree: %w", err)
	}

	return nil
}

// shouldResetDecryptedFile determines whether a file should be reset based on its decrypted content.
func shouldResetDecryptedFile(tree *object.Tree, repoRoot, file string) bool {
	fileObj, err := tree.File(file)
	if err != nil {
		return true // Not tracked, default to reset
	}

	committedBytes, err := fileObj.Contents()
	if err != nil {
		return true
	}

	if !encryption.IsEncryptedContent(committedBytes) {
		return true
	}

	format := encryption.GetFileFormat(fileObj.Name)

	decryptedContent, err := encryption.DecryptContent([]byte(committedBytes), format)
	if err != nil {
		return true
	}

	workingContent, err := os.ReadFile(filepath.Join(repoRoot, file)) // #nosec G304
	if err != nil {
		return true
	}

	return !strings.EqualFold(string(decryptedContent), string(workingContent))
}
