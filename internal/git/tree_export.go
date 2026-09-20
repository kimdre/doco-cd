package git

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

// gitmodulesFileName is the well-known name of the submodule config file,
// always located at a repository's root.
const gitmodulesFileName = ".gitmodules"

// ExportOptions configures ExportTree, in particular its optional submodule materialization.
type ExportOptions struct {
	// Log receives debug output. Optional; defaults to slog.Default().
	Log *slog.Logger

	// SubmoduleCacheDir enables submodule materialization when non-empty:
	// each resolved submodule URL gets its own dedicated mirror clone
	// under this directory, fetched once and reused across commits and
	// parent repositories that reference the same URL.
	// If empty, submodule (gitlink) entries are left as empty directories,
	// the same way `git archive` treats them.
	SubmoduleCacheDir string

	// Credentials and network options, applied both to the top-level
	// repository (already fetched by the caller) and to any submodule
	// mirrors this export fetches.
	Private                 bool
	SSHPrivateKey           string
	SSHPrivateKeyPassphrase string
	AccessToken             string
	SkipTLSVerify           bool
	ProxyOptions            transport.ProxyOptions
	// Depth bounds how much history a submodule fetch pulls; 0 fetches
	// full history. It has no effect on the top-level repository, which
	// the caller has already fetched.
	Depth int
}

// ExportTree materializes commit's tree into dir: directories, regular files
// (preserving the executable bit) and symlinks (rejecting any link whose
// target would resolve outside dir). If opts.SubmoduleCacheDir is set,
// submodules are additionally fetched and recursively exported at their
// configured path, down to DefaultSubmoduleRecursionDepth levels deep.
//
// repo must already have commit reachable in its object database (e.g. via
// a prior CloneOrUpdateBareMirror call for the reference that named it);
// ExportTree never fetches to make the top-level commit reachable, only -
// optionally - to make a submodule's pinned commit reachable in its own mirror.
func ExportTree(dir string, repo *git.Repository, commit plumbing.Hash, opts ExportOptions) error {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve export directory: %w", err)
	}

	commitObj, err := repo.CommitObject(commit)
	if err != nil {
		return fmt.Errorf("get commit %s: %w", commit, err)
	}

	tree, err := commitObj.Tree()
	if err != nil {
		return fmt.Errorf("get tree for commit %s: %w", commit, err)
	}

	var parentRemoteURL string

	var submodules map[string]*gitconfig.Submodule

	if opts.SubmoduleCacheDir != "" {
		parentRemoteURL, err = getPrimaryRemoteURL(repo)
		if err != nil {
			return fmt.Errorf("resolve parent remote for submodules: %w", err)
		}

		submodules = readGitmodules(tree)
	}

	if err := exportTree(exportCtx{
		absRoot:         absDir,
		repo:            repo,
		parentRemoteURL: parentRemoteURL,
		opts:            opts,
		depth:           int(git.DefaultSubmoduleRecursionDepth),
		submodules:      submodules,
	}, tree, ""); err != nil {
		return err
	}

	return verifyNoEscapingSymlinks(absDir)
}

// verifyNoEscapingSymlinks rejects any exported symlink whose fully resolved
// target leaves root. exportSymlink's own check is lexical, so it is blind to
// symlinks installed by other tree entries: a crafted repository can chain
// them (d/l -> "..", m -> "d/l/../..") so that every entry passes on its own
// while the resulting link graph escapes. This pass runs once the whole tree
// is materialized, so resolution observes the final graph regardless of the
// order entries were exported in.
func verifyNoEscapingSymlinks(root string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve export directory: %w", err)
	}

	return filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}

		resolved, err := resolveSymlink(p)
		if err != nil {
			return fmt.Errorf("resolve symlink %s: %w", p, err)
		}

		if !filesystem.InBasePath(realRoot, resolved) {
			return fmt.Errorf("%w: symlink %s resolves outside the export directory", filesystem.ErrPathTraversal, p)
		}

		return nil
	})
}

// resolveSymlink returns the location linkPath resolves to, following
// intermediate symlinks. Git permits dangling links, whose trailing components
// cannot be resolved; those are appended to the longest prefix that does
// resolve, which is exact because a path that does not exist cannot traverse a
// further symlink.
func resolveSymlink(linkPath string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(linkPath); err == nil {
		return resolved, nil
	}

	target, err := os.Readlink(linkPath)
	if err != nil {
		return "", err
	}

	if filepath.IsAbs(target) {
		return resolveLongestExistingPrefix(filepath.FromSlash(target)), nil
	}

	parent, err := filepath.EvalSymlinks(filepath.Dir(linkPath))
	if err != nil {
		return "", err
	}

	// Concatenated rather than joined: filepath.Join cleans ".." segments
	// textually, which would erase exactly the traversal that an intermediate
	// symlink makes real.
	return resolveLongestExistingPrefix(parent + string(filepath.Separator) + filepath.FromSlash(target)), nil
}

// resolveLongestExistingPrefix resolves symlinks in the longest existing prefix
// of path and re-appends the components that do not exist.
func resolveLongestExistingPrefix(path string) string {
	current := path

	var suffix []string

	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for _, s := range slices.Backward(suffix) {
				resolved = filepath.Join(resolved, s)
			}

			return filepath.Clean(resolved)
		}

		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(path)
		}

		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

// exportCtx carries the state that stays constant across one ExportTree
// call's recursion, so the recursive helpers below don't need long,
// repetitive parameter lists.
type exportCtx struct {
	absRoot         string
	repo            *git.Repository
	parentRemoteURL string
	opts            ExportOptions
	depth           int
	// submodules is the repository's own .gitmodules, keyed by each
	// submodule's configured path. It is read once per repository root
	// rather than per tree: .gitmodules only exists at the root and its
	// paths are root-relative, so reading it per subtree would leave every
	// submodule below the root unmatched and silently exported as an empty
	// directory.
	submodules map[string]*gitconfig.Submodule
}

// exportTree exports every entry of tree, whose logical location within
// absRoot is relPath (slash-separated, "" for the root).
func exportTree(ctx exportCtx, tree *object.Tree, relPath string) error {
	for _, entry := range tree.Entries {
		if err := validateEntryName(entry.Name); err != nil {
			return fmt.Errorf("tree entry %q: %w", path.Join(relPath, entry.Name), err)
		}

		entryRelPath := path.Join(relPath, entry.Name)
		target := filepath.Join(ctx.absRoot, filepath.FromSlash(entryRelPath))

		switch entry.Mode {
		case filemode.Dir:
			subtree, err := object.GetTree(ctx.repo.Storer, entry.Hash)
			if err != nil {
				return fmt.Errorf("read subtree %s: %w", entryRelPath, err)
			}

			if err := os.MkdirAll(target, filesystem.PermDir); err != nil {
				return fmt.Errorf("create directory %s: %w", entryRelPath, err)
			}

			if err := exportTree(ctx, subtree, entryRelPath); err != nil {
				return err
			}
		case filemode.Submodule:
			if err := os.MkdirAll(target, filesystem.PermDir); err != nil {
				return fmt.Errorf("create submodule directory %s: %w", entryRelPath, err)
			}

			if ctx.opts.SubmoduleCacheDir == "" || ctx.depth <= 0 {
				// Left as an empty directory, the same way `git archive`
				// treats submodules it isn't asked to materialize.
				continue
			}

			cfg := ctx.submodules[entryRelPath]
			if err := exportSubmodule(ctx, target, entry, cfg); err != nil {
				return fmt.Errorf("export submodule %s: %w", entryRelPath, err)
			}
		case filemode.Symlink:
			if err := exportSymlink(ctx.absRoot, target, ctx.repo, entry); err != nil {
				return fmt.Errorf("export symlink %s: %w", entryRelPath, err)
			}
		default: // Regular, Executable, Deprecated
			if err := exportFile(target, ctx.repo, entry); err != nil {
				return fmt.Errorf("export file %s: %w", entryRelPath, err)
			}
		}
	}

	return nil
}

// validateEntryName rejects tree entry names that are not a single, plain
// path segment. Well-formed Git trees never produce these, but a crafted
// tree object could; this keeps a malicious tree from writing outside dir
// regardless of what object.Tree itself tolerates.
func validateEntryName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%w: %q", filesystem.ErrPathTraversal, name)
	}

	return nil
}

// exportFile writes entry's blob content to target, preserving the executable bit.
func exportFile(target string, repo *git.Repository, entry object.TreeEntry) error {
	blob, err := object.GetBlob(repo.Storer, entry.Hash)
	if err != nil {
		return fmt.Errorf("read blob: %w", err)
	}

	r, err := blob.Reader()
	if err != nil {
		return fmt.Errorf("open blob reader: %w", err)
	}
	defer r.Close()

	perm := os.FileMode(filesystem.PermPublic)
	if entry.Mode == filemode.Executable {
		perm |= 0o111
	}

	if err := os.MkdirAll(filepath.Dir(target), filesystem.PermDir); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}

	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("write file: %w", err)
	}

	return f.Close()
}

// exportSymlink materializes entry - whose blob content is the link target
// - as a real symlink at target, rejecting any target that would resolve
// outside root.
func exportSymlink(root, target string, repo *git.Repository, entry object.TreeEntry) error {
	blob, err := object.GetBlob(repo.Storer, entry.Hash)
	if err != nil {
		return fmt.Errorf("read blob: %w", err)
	}

	r, err := blob.Reader()
	if err != nil {
		return fmt.Errorf("open blob reader: %w", err)
	}

	linkTarget, err := io.ReadAll(r)
	_ = r.Close()

	if err != nil {
		return fmt.Errorf("read link target: %w", err)
	}

	linkTargetStr := string(linkTarget)
	cleanLinkTarget := filepath.FromSlash(linkTargetStr)

	// filepath.Join does not special-case an absolute second argument - it
	// would always join and clean it relative to the link's directory,
	// making the check below pass for an absolute target while the
	// os.Symlink call still writes the real, unmangled absolute target.
	// Reject absolute targets outright instead.
	if filepath.IsAbs(cleanLinkTarget) {
		return fmt.Errorf("%w: absolute symlink target %q", filesystem.ErrPathTraversal, linkTargetStr)
	}

	resolved := filepath.Join(filepath.Dir(target), cleanLinkTarget)
	if !filesystem.InBasePath(root, resolved) {
		return fmt.Errorf("%w: symlink target %q", filesystem.ErrPathTraversal, linkTargetStr)
	}

	if err := os.MkdirAll(filepath.Dir(target), filesystem.PermDir); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	return os.Symlink(linkTargetStr, target)
}

// readGitmodules parses the root .gitmodules file, if any, into a map keyed
// by each submodule's configured path. Absence or a parse failure is not
// fatal - it just means no gitlink entry will be matched, so it is
// materialized as an empty directory instead.
func readGitmodules(tree *object.Tree) map[string]*gitconfig.Submodule {
	f, err := tree.File(gitmodulesFileName)
	if err != nil {
		return nil
	}

	content, err := f.Contents()
	if err != nil {
		return nil
	}

	m := gitconfig.NewModules()
	if err := m.Unmarshal([]byte(content)); err != nil {
		return nil
	}

	byPath := make(map[string]*gitconfig.Submodule, len(m.Submodules))
	for _, s := range m.Submodules {
		byPath[s.Path] = s
	}

	return byPath
}

// exportSubmodule fetches the submodule pinned at entry.Hash into its own
// cache mirror and recursively exports its tree into target.
func exportSubmodule(ctx exportCtx, target string, entry object.TreeEntry, cfg *gitconfig.Submodule) error {
	if cfg == nil || strings.TrimSpace(cfg.URL) == "" {
		// No (usable) .gitmodules entry for this gitlink - nothing tells us
		// where to fetch it from, so it stays an empty directory.
		return nil
	}

	resolvedURL := cfg.URL
	if isRelativeSubmoduleURL(resolvedURL) {
		var err error

		resolvedURL, err = resolveSubmoduleURL(ctx.parentRemoteURL, resolvedURL)
		if err != nil {
			return fmt.Errorf("resolve submodule URL: %w", err)
		}
	}

	mirrorDir := filepath.Join(ctx.opts.SubmoduleCacheDir, submoduleCacheKey(resolvedURL))

	// A bare mirror is all this needs: the submodule's tree is read from
	// its object database below (subRepo.CommitObject(...).Tree()) and
	// exported the same way the top-level repository is, never from a
	// checked-out working tree.
	subRepo, err := CloneOrUpdateBareMirror(ctx.opts.Log,
		resolvedURL, entry.Hash.String(), mirrorDir,
		ctx.opts.Private, ctx.opts.SSHPrivateKey, ctx.opts.SSHPrivateKeyPassphrase, ctx.opts.AccessToken,
		ctx.opts.SkipTLSVerify, ctx.opts.ProxyOptions, ctx.opts.Depth)
	if err != nil {
		return fmt.Errorf("fetch submodule %s: %w", resolvedURL, err)
	}

	commitObj, err := subRepo.CommitObject(entry.Hash)
	if err != nil {
		return fmt.Errorf("get submodule commit %s: %w", entry.Hash, err)
	}

	subTree, err := commitObj.Tree()
	if err != nil {
		return fmt.Errorf("get submodule tree %s: %w", entry.Hash, err)
	}

	subParentRemoteURL, err := getPrimaryRemoteURL(subRepo)
	if err != nil {
		return fmt.Errorf("resolve submodule remote: %w", err)
	}

	subCtx := ctx
	subCtx.absRoot = target
	subCtx.repo = subRepo
	subCtx.parentRemoteURL = subParentRemoteURL
	subCtx.depth = ctx.depth - 1
	// A nested submodule's own .gitmodules lives at its root, not the
	// parent's.
	subCtx.submodules = readGitmodules(subTree)

	return exportTree(subCtx, subTree, "")
}

// submoduleCacheKey returns a filesystem-safe cache directory name for a
// resolved submodule URL, so the same submodule is fetched once regardless
// of how many parent commits or repositories reference it.
func submoduleCacheKey(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}
