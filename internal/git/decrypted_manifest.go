package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/go-git/go-git/v5"
	gitfs "github.com/go-git/go-git/v5/storage/filesystem"

	"github.com/kimdre/doco-cd/internal/common/types/set"
)

// decryptedManifestFileName is stored inside the repository's Git directory
// (never the working tree), so it is never visible to compose auto-discovery
// or decrypt scans, and is removed automatically whenever the repository
// directory itself is removed (e.g. on re-clone).
const decryptedManifestFileName = "doco-cd-decrypted-manifest.json"

// decryptedFilesManifest is the on-disk representation of the set of
// repository-root-relative files that were decrypted in place, tagged with
// the commit the working tree was at when they were decrypted. Keying by
// commit means a checkout to a different commit automatically invalidates
// the manifest: see ReadDecryptedFilesManifest.
type decryptedFilesManifest struct {
	Commit string   `json:"commit"`
	Files  []string `json:"files"`
}

// WriteDecryptedFilesManifest records which files under repoRoot were
// decrypted in place (by internal/docker's Compose loader), tagged with
// repoRoot's current HEAD commit. files may be absolute or repoRoot-relative;
// they are normalized to slash-separated repoRoot-relative paths.
//
// It is a no-op (returns nil) when repoRoot is not a Git working tree or has
// no resolvable HEAD, since the manifest is only ever consulted by
// ResetTrackedFiles for Git-managed repositories.
func WriteDecryptedFilesManifest(repoRoot string, files []string) error {
	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		return nil //nolint:nilerr // non-git working directories have nothing to record
	}

	head, err := repo.Head()
	if err != nil {
		return nil //nolint:nilerr // no resolvable HEAD (e.g. an empty repository): nothing to record
	}

	manifestPath, err := decryptedManifestPath(repo)
	if err != nil {
		return nil //nolint:nilerr // not filesystem-backed storage: nowhere to persist the manifest
	}

	relFiles := make([]string, 0, len(files))

	for _, f := range files {
		rel := f

		if filepath.IsAbs(f) {
			rel, err = filepath.Rel(repoRoot, f)
			if err != nil {
				continue
			}
		}

		relFiles = append(relFiles, filepath.ToSlash(rel))
	}

	sort.Strings(relFiles)

	data, err := json.Marshal(decryptedFilesManifest{Commit: head.Hash().String(), Files: relFiles})
	if err != nil {
		return fmt.Errorf("marshal decrypted-files manifest: %w", err)
	}

	dir := filepath.Dir(manifestPath)

	tmp, err := os.CreateTemp(dir, ".decrypted-manifest-*")
	if err != nil {
		return fmt.Errorf("create temporary decrypted-files manifest: %w", err)
	}

	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write decrypted-files manifest: %w", err)
	}

	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync decrypted-files manifest: %w", err)
	}

	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close decrypted-files manifest: %w", err)
	}

	if err = os.Rename(tmpPath, manifestPath); err != nil {
		return fmt.Errorf("replace decrypted-files manifest: %w", err)
	}

	return nil
}

// ReadDecryptedFilesManifest returns the set of repository-root-relative
// files recorded as decrypted in place for repo, provided the manifest was
// recorded for the exact commit currently at repo's HEAD. Any other
// situation - no manifest, a manifest recorded for a different commit, or a
// read/parse error - returns an empty set: the caller should treat that
// identically to "nothing recorded" and fall back to resetting everything.
func ReadDecryptedFilesManifest(repo *git.Repository) set.Set[string] {
	head, err := repo.Head()
	if err != nil {
		return set.New[string]()
	}

	manifestPath, err := decryptedManifestPath(repo)
	if err != nil {
		return set.New[string]()
	}

	data, err := os.ReadFile(manifestPath) // #nosec G304 -- manifestPath is derived from the repository's own resolved Git directory
	if err != nil {
		return set.New[string]()
	}

	var manifest decryptedFilesManifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		return set.New[string]()
	}

	if manifest.Commit != head.Hash().String() {
		return set.New[string]()
	}

	return set.New(manifest.Files...)
}

// decryptedManifestPath returns the manifest path for repo, resolved inside
// its Git directory. Using the Storer's resolved filesystem (rather than
// naively joining ".git" onto the worktree root) transparently handles linked
// worktrees and submodules, whose ".git" is a file pointing elsewhere:
// go-git already resolves that indirection when the repository is opened.
func decryptedManifestPath(repo *git.Repository) (string, error) {
	storage, ok := repo.Storer.(*gitfs.Storage)
	if !ok {
		return "", errors.New("repository storage is not filesystem-backed")
	}

	return filepath.Join(storage.Filesystem().Root(), decryptedManifestFileName), nil
}
