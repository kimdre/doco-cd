package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5/config"

	"github.com/kimdre/doco-cd/internal/git"
)

// maxRepoSearchDepth bounds how deep below the data directory a store base
// directory is looked for. Repository names are "<host>/<owner>/<repo>", one
// level deeper for nested groups (e.g. GitLab subgroups); the cap keeps a walk
// from descending into whatever else lives on the data volume.
const maxRepoSearchDepth = 6

// repoRootMarkers are the subdirectory names that identify a directory as a
// store base directory: the bare mirror clone of a Git source, and the
// published artifacts both source types write.
var repoRootMarkers = []string{ArtifactsSubdir, MirrorSubdir}

// ListRepositoryDirs returns the absolute path of every store base directory
// beneath dataDir, or none if dataDir does not exist.
//
// A store base directory is not an immediate child of dataDir: git.GetRepoName
// (and the OCI equivalent) produce a "<host>/<owner>/<repo>" path, so base
// directories are found by walking - the same way internal/migration locates
// legacy repository roots - and recognizing a directory that holds an
// "artifacts" or "mirror" subdirectory. Descent stops at a base directory, so
// artifact contents are never walked, and unreadable subtrees are skipped.
func ListRepositoryDirs(dataDir string) ([]string, error) {
	if _, err := os.Stat(dataDir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, err
	}

	var dirs []string

	err := filepath.WalkDir(dataDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An unreadable subtree must not abort the walk of the others.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}

			return nil //nolint:nilerr // a single unreadable entry is skipped, not fatal
		}

		if !d.IsDir() || path == dataDir {
			return nil
		}

		if isRepoRoot(path) {
			dirs = append(dirs, path)
			return filepath.SkipDir
		}

		rel, err := filepath.Rel(dataDir, path)
		if err != nil || strings.Count(filepath.ToSlash(rel), "/")+1 >= maxRepoSearchDepth {
			return filepath.SkipDir
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return dirs, nil
}

// isRepoRoot reports whether dir is a store base directory.
func isRepoRoot(dir string) bool {
	for _, marker := range repoRootMarkers {
		if info, err := os.Stat(filepath.Join(dir, marker)); err == nil && info.IsDir() {
			return true
		}
	}

	return false
}

// Mirror is a bare Git mirror kept by a GitStore.
type Mirror struct {
	// Repository is the mirrored repository in "<host>/<owner>/<repo>" form.
	// A repository used as a submodule by several stores has a mirror in each.
	Repository string
	// Path is the mirror's directory.
	Path string
}

// ListMirrors returns the bare Git mirrors of every store beneath dataDir,
// sorted by repository and path: each store's mirror of its own repository and
// the mirrors of the submodules its revisions use.
func ListMirrors(dataDir string) ([]Mirror, error) {
	roots, err := ListRepositoryDirs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("list repository directories: %w", err)
	}

	var mirrors []Mirror

	for _, root := range roots {
		if mirrorDir := filepath.Join(root, MirrorSubdir); isBareRepository(mirrorDir) {
			// A store's base directory is its repository's name below the data
			// directory, the name git.GetRepoName derives from the clone URL.
			rel, err := filepath.Rel(dataDir, root)
			if err != nil {
				return nil, fmt.Errorf("locate mirror %s: %w", mirrorDir, err)
			}

			mirrors = append(mirrors, Mirror{Repository: filepath.ToSlash(rel), Path: mirrorDir})
		}

		submodules := filepath.Join(root, SubmodulesSubdir)

		entries, err := os.ReadDir(submodules)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}

			return nil, fmt.Errorf("list submodule mirrors: %w", err)
		}

		for _, entry := range entries {
			dir := filepath.Join(submodules, entry.Name())
			if !entry.IsDir() || !isBareRepository(dir) {
				continue
			}

			// A mirror without a remote is still being cloned.
			if repository := mirrorRepository(dir); repository != "" {
				mirrors = append(mirrors, Mirror{Repository: repository, Path: dir})
			}
		}
	}

	slices.SortFunc(mirrors, func(a, b Mirror) int {
		if c := strings.Compare(a.Repository, b.Repository); c != 0 {
			return c
		}

		return strings.Compare(a.Path, b.Path)
	})

	return mirrors, nil
}

// isBareRepository reports whether dir looks like a bare Git repository.
func isBareRepository(dir string) bool {
	if info, err := os.Stat(filepath.Join(dir, "objects")); err != nil || !info.IsDir() {
		return false
	}

	info, err := os.Stat(filepath.Join(dir, "HEAD"))

	return err == nil && info.Mode().IsRegular()
}

// mirrorRepository returns the repository the mirror at dir was cloned from,
// or "" if its configuration names none.
//
// It reads the configuration without the mirror's lock. Submodule mirrors are
// named after their URL, so their remote never changes once written.
func mirrorRepository(dir string) string {
	f, err := os.Open(filepath.Join(dir, "config")) // #nosec G304 -- dir is a mirror below the data directory.
	if err != nil {
		return ""
	}

	defer func() { _ = f.Close() }()

	cfg, err := config.ReadConfig(f)
	if err != nil {
		return ""
	}

	remote, ok := cfg.Remotes[git.RemoteName]
	if !ok || len(remote.URLs) == 0 {
		return ""
	}

	return git.GetRepoName(remote.URLs[0])
}
