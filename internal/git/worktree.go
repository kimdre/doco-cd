package git

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

// ResetTrackedFiles resets tracked files to their last committed state, leaving untracked files intact.
//
// Files holding SOPS content that was decrypted in place are restored straight from their committed blob,
// so the ciphertext is never written to the worktree and anything watching those files is not woken.
// Repairing them here rather than relying on the next per-stack compose load keeps the repo-wide reset and
// the repair symmetric:
// a stack that is skipped, filtered out or interrupted would otherwise keep ciphertext on disk indefinitely.
func ResetTrackedFiles(repo *git.Repository) error {
	return resetTrackedFiles(repo, true)
}

// resetTrackedFilesWithoutRestore resets tracked files like ResetTrackedFiles,
// but leaves decrypted files encrypted and the decrypted-files manifest untouched.
// It is meant for the moments where the worktree has to be clean against its index,
// most notably right before a submodule checkout, which refuses to run with unstaged changes.
// Callers are expected to run ResetTrackedFiles afterward to restore the plaintext again.
func resetTrackedFilesWithoutRestore(repo *git.Repository) error {
	return resetTrackedFiles(repo, false)
}

// resetTrackedFiles resets tracked files to their last committed state,
// optionally restoring decrypted files from their committed blob.
func resetTrackedFiles(repo *git.Repository, restoreDecrypted bool) error {
	worktree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	changedFiles, err := worktree.Status()
	if err != nil {
		return fmt.Errorf("failed to get worktree status: %w", err)
	}

	root := worktree.Filesystem.Root()

	resetFiles := make([]string, 0, len(changedFiles))

	for file, status := range changedFiles {
		// Do not touch files that are not part of the Git repository (e.g. created by a container process)
		if status.Staging == git.Untracked {
			continue
		}

		resetFiles = append(resetFiles, file)
	}

	if !restoreDecrypted {
		return resetFilesInWorktree(worktree, resetFiles)
	}

	// Recorded entries are kept regardless of the commit they were tagged with:
	// a file recorded as decrypted but currently clean means an earlier run was
	// interrupted before it could be decrypted again, and it needs the same
	// repair as a file that is decrypted right now.
	candidates := ReadDecryptedFilesManifestAny(repo)

	tree, treeErr := headTree(repo)
	if treeErr != nil {
		// Without a HEAD tree nothing can be restored, and leaving the manifest
		// untouched keeps the next run able to repair the recorded files.
		return resetFilesInWorktree(worktree, resetFiles)
	}

	for _, file := range resetFiles {
		if !candidates.Contains(file) && isDecryptedInPlace(tree, root, file) {
			candidates.Add(file)
		}
	}

	// Restore the plaintext straight from the committed blob before resetting,
	// so the ciphertext is never written to the worktree at all. Anything that
	// could not be restored stays in resetFiles and is reset as usual: keeping
	// the plaintext of an older commit around would be worse than ciphertext.
	result := restoreDecryptedFiles(tree, root, candidates)

	resetFiles = slices.DeleteFunc(resetFiles, result.restored.Contains)

	if err = resetFilesInWorktree(worktree, resetFiles); err != nil {
		return err
	}

	if candidates.IsEmpty() {
		// Nothing is or ever was decrypted here, so there is no manifest to
		// create or maintain. Repositories without encrypted files never grow one.
		return nil
	}

	if err = replaceDecryptedFilesManifest(repo, result.recorded()); err != nil {
		slog.Warn("failed to persist decrypted-files manifest after reset",
			slog.String("path", root), slog.Any("error", err))
	}

	return nil
}

// headTree returns the tree of the currently checked out commit.
func headTree(repo *git.Repository) (*object.Tree, error) {
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve HEAD: %w", err)
	}

	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to resolve HEAD commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve HEAD tree: %w", err)
	}

	return tree, nil
}

// maxAutoDetectSize bounds the committed blob size doco-cd reads and parses to
// find out whether a changed file is a SOPS document that was decrypted in
// place. SOPS documents are configuration files, so this is far above anything
// realistic, while it keeps a single large asset in a commit from being read
// into memory and run through five parsers on every checkout. Files recorded in
// the decrypted-files manifest bypass this limit, as they are known to be SOPS
// documents.
const maxAutoDetectSize = 8 << 20 // 8 MiB

// isDecryptedInPlace reports whether file looks like a committed SOPS document
// whose worktree copy has been decrypted in place. Only SOPS metadata is
// inspected, so no decryption and no SOPS key are required.
func isDecryptedInPlace(tree *object.Tree, root, file string) bool {
	entry, err := tree.File(file)
	if err != nil || entry.Size > maxAutoDetectSize {
		return false
	}

	committed, err := readTreeFile(entry, file)
	if err != nil || !mayContainSopsMetadata(committed) {
		return false
	}

	if _, encrypted := encryption.DetectFormat(committed, file); !encrypted {
		return false
	}

	current, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file))) // #nosec G304 -- file is a repository-relative path reported by the worktree status
	if err != nil {
		return false
	}

	_, stillEncrypted := encryption.DetectFormat(current, file)

	return !stillEncrypted
}

// mayContainSopsMetadata is a cheap necessary condition for SOPS content: every
// format SOPS emits stores its metadata under keys prefixed with "sops". It only
// rules content out, the authoritative check stays with encryption.DetectFormat.
func mayContainSopsMetadata(content []byte) bool {
	return bytes.Contains(content, []byte("sops"))
}

// treeFileContents returns the committed contents of file in tree.
func treeFileContents(tree *object.Tree, file string) ([]byte, error) {
	entry, err := tree.File(file)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve %s in HEAD tree: %w", file, err)
	}

	return readTreeFile(entry, file)
}

// readTreeFile reads the full contents of an already resolved tree entry.
func readTreeFile(entry *object.File, file string) ([]byte, error) {
	reader, err := entry.Reader()
	if err != nil {
		return nil, fmt.Errorf("failed to read %s from HEAD tree: %w", file, err)
	}

	defer func() { _ = reader.Close() }()

	contents, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s from HEAD tree: %w", file, err)
	}

	return contents, nil
}

// restoreResult reports the outcome of restoring decrypted files.
type restoreResult struct {
	// restored holds the files whose plaintext is now on disk. They must not be
	// reset, as that would overwrite the plaintext with the committed ciphertext.
	restored set.Set[string]
	// retry holds the files that are SOPS documents but could not be decrypted
	// right now, for example because the SOPS key was not available yet. They
	// are reset, but stay recorded so a later run tries again.
	retry set.Set[string]
}

// recorded returns the files to store in the decrypted-files manifest.
func (r restoreResult) recorded() set.Set[string] {
	recorded := set.New[string]()

	for file := range r.restored {
		recorded.Add(file)
	}

	for file := range r.retry {
		recorded.Add(file)
	}

	return recorded
}

// restoreDecryptedFiles writes the decrypted form of each candidate straight
// from its committed blob and reports what it restored. Candidates that are no
// longer committed or no longer SOPS documents are dropped, so they end up reset
// to their committed state instead.
//
// Failures are logged rather than returned: a checkout must not fail because
// secrets cannot be decrypted, and the per-stack compose load still reports the
// same problem for the stack it belongs to. A file that fails to decrypt stays
// recorded, because nothing else can classify it once the reset has put the
// ciphertext back on disk.
func restoreDecryptedFiles(tree *object.Tree, root string, candidates set.Set[string]) restoreResult {
	result := restoreResult{restored: set.New[string](), retry: set.New[string]()}

	for file := range candidates {
		path := filepath.Join(root, filepath.FromSlash(file))
		if !filesystem.InBasePath(root, path) {
			continue
		}

		committed, err := treeFileContents(tree, file)
		if err != nil {
			continue
		}

		decrypted, err := encryption.DecryptToFile(path, committed)
		if err != nil {
			slog.Warn("failed to decrypt file while resetting tracked files",
				slog.String("file", file), slog.String("path", root), slog.Any("error", err))

			result.retry.Add(file)

			continue
		}

		if decrypted {
			result.restored.Add(file)
		}
	}

	return result
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
