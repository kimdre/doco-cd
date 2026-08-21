package git

import (
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	gitfs "github.com/go-git/go-git/v5/storage/filesystem"
)

const (
	// gitDirPrefix marks the target directory inside a ".git" file, which is used instead
	// of a ".git" directory by linked worktrees and by submodules with a shared object store.
	gitDirPrefix      = "gitdir:"
	gitDirName        = ".git"
	maxGitDirFileSize = 4096
)

// localRepoLoader resolves a file:// endpoint path to a storer.Storer.
//
// It replaces go-git's built-in file transport, which shells out to the
// git-upload-pack/git-receive-pack binaries, so doco-cd never needs a git binary to
// poll and deploy Git repositories that live on the local filesystem.
type localRepoLoader struct {
	base billy.Filesystem
}

// Load implements server.Loader. It accepts bare repositories (objects and refs directly
// under the endpoint path), standard repositories (a ".git" directory) and linked
// worktrees or submodules (a ".git" file pointing at the real git directory).
func (l *localRepoLoader) Load(ep *transport.Endpoint) (storer.Storer, error) {
	if ep == nil || ep.Path == "" {
		return nil, transport.ErrRepositoryNotFound
	}

	gitDir, err := l.resolveGitDir(ep.Path)
	if err != nil {
		return nil, err
	}

	return gitfs.NewStorage(gitDir, cache.NewObjectLRUDefault()), nil
}

// resolveGitDir returns the filesystem holding the git directory for a repository path.
func (l *localRepoLoader) resolveGitDir(repoPath string) (billy.Filesystem, error) {
	fs, err := l.base.Chroot(repoPath)
	if err != nil {
		return nil, err
	}

	// A "config" file at the root means the path already points at a git directory
	// (a bare repository, or a git directory referenced directly).
	if _, err = fs.Stat("config"); err == nil {
		return fs, nil
	}

	info, err := fs.Stat(gitDirName)
	if err != nil {
		return nil, transport.ErrRepositoryNotFound
	}

	if info.IsDir() {
		return fs.Chroot(gitDirName)
	}

	// ".git" is a file containing "gitdir: <path>" (linked worktree or submodule).
	target, err := readGitDirFile(fs, gitDirName)
	if err != nil {
		return nil, err
	}

	if !path.IsAbs(target) {
		target = path.Join(repoPath, target)
	}

	gitDir, err := l.base.Chroot(target)
	if err != nil {
		return nil, err
	}

	if _, err = gitDir.Stat("config"); err != nil {
		// Worktrees keep their own git directory but share the main repository's
		// object store and refs, which are reachable through "commondir".
		commonDir, commonErr := readCommonDirFile(gitDir, target)
		if commonErr != nil {
			return nil, transport.ErrRepositoryNotFound
		}

		return l.base.Chroot(commonDir)
	}

	return gitDir, nil
}

// readGitDirFile reads a ".git" file and returns the git directory it points to.
func readGitDirFile(fs billy.Filesystem, name string) (string, error) {
	content, err := readSmallFile(fs, name)
	if err != nil {
		return "", err
	}

	if !strings.HasPrefix(content, gitDirPrefix) {
		return "", fmt.Errorf("%w: malformed %s file", transport.ErrRepositoryNotFound, name)
	}

	target := strings.TrimSpace(strings.TrimPrefix(content, gitDirPrefix))
	if target == "" {
		return "", fmt.Errorf("%w: empty gitdir in %s file", transport.ErrRepositoryNotFound, name)
	}

	return target, nil
}

// readCommonDirFile resolves a worktree's "commondir" pointer to the main git directory.
func readCommonDirFile(fs billy.Filesystem, gitDir string) (string, error) {
	content, err := readSmallFile(fs, "commondir")
	if err != nil {
		return "", err
	}

	commonDir := strings.TrimSpace(content)
	if commonDir == "" {
		return "", fmt.Errorf("%w: empty commondir file", transport.ErrRepositoryNotFound)
	}

	if !path.IsAbs(commonDir) {
		commonDir = path.Join(gitDir, commonDir)
	}

	return commonDir, nil
}

// readSmallFile reads a short metadata file, guarding against oversized input.
func readSmallFile(fs billy.Filesystem, name string) (string, error) {
	f, err := fs.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only file

	content, err := io.ReadAll(io.LimitReader(f, maxGitDirFileSize))
	if err != nil {
		return "", err
	}

	return string(content), nil
}

// init registers a pure-Go, in-process implementation of the "file" transport scheme,
// overriding go-git's default subprocess-based implementation. This enables
// cloning/fetching from local filesystem Git repositories (file:// URLs) without
// requiring a git binary on PATH.
func init() {
	loader := &localRepoLoader{base: osfs.New("/")}
	gitclient.InstallProtocol("file", server.NewClient(loader))
}
