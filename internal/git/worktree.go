package git

import (
	"fmt"

	"github.com/go-git/go-git/v5"
)

// ResetTrackedFiles resets all tracked files in the worktree To their last committed state
// while leaving untracked files intact. A file that was decrypted in place for the exact
// commit currently checked out (recorded via WriteDecryptedFilesManifest) is left alone
// instead of being reset back to its encrypted committed form.
func ResetTrackedFiles(repo *git.Repository) error {
	worktree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	changedFiles, err := worktree.Status()
	if err != nil {
		return fmt.Errorf("failed to get worktree status: %w", err)
	}

	decryptedFiles := ReadDecryptedFilesManifest(repo)

	resetFiles := make([]string, 0, len(changedFiles))

	for file, status := range changedFiles {
		// Do not touch files that are not part of the Git repository (e.g. created by a container process)
		if status.Staging == git.Untracked {
			continue
		}

		if decryptedFiles.Contains(file) {
			continue
		}

		resetFiles = append(resetFiles, file)
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
