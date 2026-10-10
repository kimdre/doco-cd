package git

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/filesystem/dotgit"
)

// refNamespaceError prevents a failed local repair from selecting a tag or re-cloning.
type refNamespaceError struct {
	cause error
}

func (e *refNamespaceError) Error() string {
	return "git reference namespace recovery failed: " + e.cause.Error()
}
func (e *refNamespaceError) Unwrap() error { return e.cause }

// isRefNamespaceError returns true if the error is a refNamespaceError.
func isRefNamespaceError(err error) bool {
	_, ok := errors.AsType[*refNamespaceError](err)
	return ok
}

// referenceFilesystem returns the repository's reference filesystem,
// or an error if the repository does not support a filesystem.
func referenceFilesystem(repo *git.Repository) (billy.Filesystem, error) {
	storage, ok := repo.Storer.(interface{ Filesystem() billy.Filesystem })
	if !ok {
		return nil, errors.New("reference storage has no filesystem")
	}

	return storage.Filesystem(), nil
}

// fetchWithRefNamespaceRecovery attempts to fetch from the remote, and if it fails due to a reference namespace collision,
// it attempts to repair the reference namespace and retry the fetch.
func fetchWithRefNamespaceRecovery(
	repo *git.Repository,
	opts *git.FetchOptions,
	fetch func() error,
	list func() ([]*plumbing.Reference, error),
) error {
	err := fetch()
	if err == nil || (!errors.Is(err, syscall.EISDIR) && !errors.Is(err, syscall.ENOTDIR) && !errors.Is(err, dotgit.ErrIsDir)) {
		return err
	}

	if repairErr := repairRefNamespace(repo, opts.RefSpecs, err, list); repairErr != nil {
		return &refNamespaceError{cause: errors.Join(err, repairErr)}
	}

	// The repair budget is one pass. A new collision must fail instead of looping.
	if retryErr := fetch(); retryErr != nil {
		return &refNamespaceError{cause: errors.Join(err, fmt.Errorf("fetch after namespace cleanup: %w", retryErr))}
	}

	return nil
}

// refNamespaceCleanup tracks the state of a reference namespace cleanup operation.
type refNamespaceCleanup struct {
	fs   billy.Filesystem
	refs map[plumbing.ReferenceName]bool
	dirs map[string]bool
	live map[plumbing.ReferenceName]bool
}

// repairRefNamespace attempts to repair a reference namespace collision by removing stale references and empty directories.
func repairRefNamespace(repo *git.Repository, specs []config.RefSpec, fetchErr error, list func() ([]*plumbing.Reference, error)) error {
	fs, err := referenceFilesystem(repo)
	if err != nil {
		return err
	}

	failedPath, ok := errors.AsType[*os.PathError](fetchErr)
	if !ok {
		return errors.New("namespace error has no filesystem path")
	}

	if err := requireRefDirectory(fs, "refs"); err != nil {
		return err
	}

	// Chroot selects the common reference store for linked worktrees as well.
	refFS, err := fs.Chroot("refs")
	if err != nil {
		return fmt.Errorf("locate reference filesystem: %w", err)
	}

	name, err := namespaceErrorReference(refFS, failedPath.Path)
	if err != nil {
		return err
	}

	if !fetchPlanCoversNamespacePath(specs, name) {
		return fmt.Errorf("failed path %s is outside the fetch destinations", name)
	}

	advertised, err := list()
	if err != nil {
		return fmt.Errorf("discover upstream references for namespace repair: %w", err)
	}

	cleanup := refNamespaceCleanup{
		fs: fs, refs: make(map[plumbing.ReferenceName]bool),
		dirs: make(map[string]bool), live: make(map[plumbing.ReferenceName]bool),
	}

	destinations := make(map[plumbing.ReferenceName]bool)

	for _, ref := range advertised {
		if !ref.Name().IsSafe() || ref.Name().Validate() != nil {
			continue
		}

		cleanup.live[ref.Name()] = true
		if ref.Type() != plumbing.HashReference || ref.Hash() == plumbing.ZeroHash {
			continue
		}

		for _, spec := range specs {
			if validNamespaceRefSpec(spec) && spec.Match(ref.Name()) {
				destinations[spec.Dst(ref.Name())] = true
			}
		}
	}

	covered := false

	var blocked []plumbing.ReferenceName

	for destination := range destinations {
		if destination == name || strings.HasPrefix(destination.String(), name.String()+"/") {
			covered = true
		}

		collision, err := cleanup.inspectDestination(destination)
		if err != nil {
			return err
		}

		if collision {
			blocked = append(blocked, destination)
		}
	}

	if !covered {
		return fmt.Errorf("upstream no longer advertises a destination for %s", name)
	}

	if len(blocked) == 0 {
		return fmt.Errorf("no obstructing reference files or directories found for %s", name)
	}

	packed, err := readNamespacePackedRefs(fs)
	if err != nil {
		return err
	}

	for _, ref := range packed {
		for _, destination := range blocked {
			if strings.HasPrefix(ref.Name().String(), destination.String()+"/") {
				if err := cleanup.addStaleRef(ref.Name()); err != nil {
					return err
				}
			}
		}
	}

	refs := slices.Sorted(maps.Keys(cleanup.refs))
	dirs := slices.Sorted(maps.Keys(cleanup.dirs))
	slices.SortFunc(dirs, func(a, b string) int { return len(b) - len(a) })

	var loose, packedOnly []plumbing.ReferenceName

	for _, ref := range refs {
		info, statErr := fs.Lstat(ref.String())
		if errors.Is(statErr, os.ErrNotExist) || errors.Is(statErr, syscall.ENOTDIR) || (statErr == nil && info.IsDir()) {
			packedOnly = append(packedOnly, ref)
			continue
		}

		if statErr != nil {
			return fmt.Errorf("inspect stale reference %s before removal: %w", ref, statErr)
		}

		if !info.Mode().IsRegular() {
			return fmt.Errorf("stale reference %s is not a regular file", ref)
		}

		loose = append(loose, ref)
	}

	slog.Info("repairing git reference namespace",
		slog.String("path", fs.Root()),
		slog.String("reference", name.String()),
		slog.Any("stale_references", refs),
		slog.Any("empty_directories", dirs))

	// Remove loose blockers before directories and packed-only descendants.
	// RemoveReference also removes a loose blocker's packed entry.
	for _, ref := range loose {
		if err := repo.Storer.RemoveReference(ref); err != nil {
			return fmt.Errorf("remove stale reference %s: %w", ref, err)
		}
	}

	for _, dir := range dirs {
		if err := requireRefDirectory(fs, dir); err != nil {
			return err
		}

		entries, err := fs.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("inspect blocking directory %s: %w", dir, err)
		}

		if len(entries) != 0 {
			return fmt.Errorf("blocking directory %s is not empty", dir)
		}

		if err := fs.Remove(dir); err != nil {
			return fmt.Errorf("remove empty reference directory %s: %w", dir, err)
		}
	}

	for _, ref := range packedOnly {
		if err := repo.Storer.RemoveReference(ref); err != nil {
			return fmt.Errorf("remove packed stale reference %s: %w", ref, err)
		}
	}

	slog.Info("repaired git reference namespace",
		slog.String("path", fs.Root()),
		slog.String("reference", name.String()))

	return nil
}

// namespaceErrorReference converts a filesystem path to a reference name, ensuring it is within the repository's reference namespace.
func namespaceErrorReference(refFS billy.Filesystem, filename string) (plumbing.ReferenceName, error) {
	if filename != filepath.Clean(filename) {
		return "", fmt.Errorf("unclean reference error path %q", filename)
	}

	// OS filesystems report error paths in the same form as their root.
	// A repository opened with a relative path reports paths relative to the working directory.
	root, rootErr := filepath.Abs(refFS.Root())

	path, pathErr := filepath.Abs(filename)
	if err := errors.Join(rootErr, pathErr); err != nil {
		return "", fmt.Errorf("locate failed reference: %w", err)
	}

	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("failed path %q is outside the reference store", filename)
	}

	ref := plumbing.ReferenceName("refs/" + filepath.ToSlash(relative))
	if !ref.IsSafe() || namespaceSource(ref) == "" {
		return "", fmt.Errorf("failed path %q is outside the owned reference namespaces", filename)
	}

	return ref, nil
}

// namespaceSource returns the source reference name for a given destination reference name,
// if it is within the repository's reference namespace.
func namespaceSource(name plumbing.ReferenceName) plumbing.ReferenceName {
	if !name.IsSafe() || name.Validate() != nil {
		return ""
	}

	if suffix, ok := strings.CutPrefix(name.String(), "refs/remotes/"+RemoteName+"/"); ok && suffix != "" {
		return plumbing.NewBranchReferenceName(suffix)
	}

	if strings.HasPrefix(name.String(), TagPrefix) && name.String() != TagPrefix {
		return name
	}

	return ""
}

// validNamespaceRefSpec returns true if the refspec is a valid fetch destination for a reference namespace.
func validNamespaceRefSpec(spec config.RefSpec) bool {
	if spec.Validate() != nil {
		return false
	}

	source, destination, _ := strings.Cut(strings.TrimPrefix(spec.String(), "+"), ":")
	switch {
	case spec == refSpecAllBranches || spec == refSpecAllTags:
		return true
	case spec.IsWildcard():
		return false
	default:
		name := plumbing.ReferenceName(destination)
		return name.IsSafe() && namespaceSource(name) == plumbing.ReferenceName(source)
	}
}

// fetchPlanCoversNamespacePath returns true if the fetch refspecs cover the given reference name.
func fetchPlanCoversNamespacePath(specs []config.RefSpec, name plumbing.ReferenceName) bool {
	for _, spec := range specs {
		if !validNamespaceRefSpec(spec) {
			continue
		}

		if spec.IsWildcard() {
			if spec.Match(namespaceSource(name)) {
				return true
			}
		} else {
			destination := spec.Dst("")
			if destination == name || strings.HasPrefix(destination.String(), name.String()+"/") {
				return true
			}
		}
	}

	return false
}

// requireRefDirectory checks that the given path exists and is a directory in the reference filesystem.
func requireRefDirectory(fs billy.Filesystem, name string) error {
	info, err := fs.Lstat(name)
	if err != nil {
		return fmt.Errorf("inspect reference directory %s: %w", name, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("reference directory %s is not a directory", name)
	}

	return nil
}

// addStaleRef adds a reference name to the cleanup's list of stale references, ensuring it is safe and not live upstream.
func (c *refNamespaceCleanup) addStaleRef(name plumbing.ReferenceName) error {
	source := namespaceSource(name)
	if !name.IsSafe() || source == "" || !source.IsSafe() {
		return fmt.Errorf("unsafe blocking reference %s", name)
	}

	if c.live[source] {
		return fmt.Errorf("blocking reference %s still exists upstream", name)
	}

	c.refs[name] = true

	return nil
}

// inspectDestination checks if the given reference name is a loose reference or directory that blocks the fetch destination.
func (c *refNamespaceCleanup) inspectDestination(name plumbing.ReferenceName) (bool, error) {
	if !name.IsSafe() || namespaceSource(name) == "" {
		return false, fmt.Errorf("unsafe fetch destination %s", name)
	}

	parts := strings.Split(name.String(), "/")
	for i := range parts {
		current := strings.Join(parts[:i+1], "/")

		info, err := c.fs.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		if err != nil {
			return false, fmt.Errorf("inspect fetch destination %s: %w", current, err)
		}

		if info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("fetch destination contains symlink %s", current)
		}

		if !info.IsDir() {
			if i == len(parts)-1 {
				if !info.Mode().IsRegular() {
					return false, fmt.Errorf("fetch destination %s is not a regular file", current)
				}

				return false, nil
			}

			if err := c.inspectLooseBlocker(plumbing.ReferenceName(current)); err != nil {
				return false, err
			}

			return true, nil
		}

		if i == len(parts)-1 {
			return true, c.inspectBlockingDirectory(current)
		}
	}

	return false, nil
}

// inspectLooseBlocker checks if the given reference name is a loose reference that blocks the fetch destination,
// and adds it to the list of stale references if so.
func (c *refNamespaceCleanup) inspectLooseBlocker(name plumbing.ReferenceName) error {
	if err := c.addStaleRef(name); err != nil {
		return err
	}

	info, err := c.fs.Lstat(name.String())
	if err != nil {
		return fmt.Errorf("inspect blocking reference %s: %w", name, err)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("blocking reference %s is not a regular file", name)
	}

	file, err := c.fs.Open(name.String())
	if err != nil {
		return fmt.Errorf("read blocking reference %s: %w", name, err)
	}

	content, readErr := io.ReadAll(io.LimitReader(file, 43))

	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return fmt.Errorf("read blocking reference %s: %w", name, err)
	}

	hash := strings.TrimSpace(string(content))
	if len(content) > 42 || !plumbing.IsHash(hash) || plumbing.NewHash(hash) == plumbing.ZeroHash {
		return fmt.Errorf("blocking file %s is not a hash reference", name)
	}

	return nil
}

// inspectBlockingDirectory checks if the given directory blocks the fetch destination.
func (c *refNamespaceCleanup) inspectBlockingDirectory(name string) error {
	if !plumbing.ReferenceName(name).IsSafe() || namespaceSource(plumbing.ReferenceName(name)) == "" {
		return fmt.Errorf("unsafe blocking directory %s", name)
	}

	if err := requireRefDirectory(c.fs, name); err != nil {
		return err
	}

	entries, err := c.fs.ReadDir(name)
	if err != nil {
		return fmt.Errorf("inspect blocking directory %s: %w", name, err)
	}

	for _, entry := range entries {
		child := name + "/" + entry.Name()

		info, err := c.fs.Lstat(child)
		if err != nil {
			return fmt.Errorf("inspect blocking path %s: %w", child, err)
		}

		if info.IsDir() {
			if err := c.inspectBlockingDirectory(child); err != nil {
				return err
			}
		} else if err := c.inspectLooseBlocker(plumbing.ReferenceName(child)); err != nil {
			return err
		}
	}

	c.dirs[name] = true

	return nil
}

// readNamespacePackedRefs reads packed references separately so enumeration cannot follow unrelated loose symlinks.
func readNamespacePackedRefs(fs billy.Filesystem) (_ []*plumbing.Reference, err error) {
	info, err := fs.Lstat("packed-refs")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("inspect packed references: %w", err)
	}

	if !info.Mode().IsRegular() {
		return nil, errors.New("packed references are not a regular file")
	}

	file, err := fs.Open("packed-refs")
	if err != nil {
		return nil, fmt.Errorf("open packed references: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()

	var refs []*plumbing.Reference

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "^") && plumbing.IsHash(line[1:]) {
			continue
		}

		hash, name, ok := strings.Cut(line, " ")

		ref := plumbing.ReferenceName(name)
		if !ok || !plumbing.IsHash(hash) || plumbing.NewHash(hash) == plumbing.ZeroHash || !ref.IsSafe() || ref.Validate() != nil {
			return nil, errors.New("malformed packed reference during namespace repair")
		}

		refs = append(refs, plumbing.NewHashReference(ref, plumbing.NewHash(hash)))
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read packed references: %w", err)
	}

	return refs, nil
}
