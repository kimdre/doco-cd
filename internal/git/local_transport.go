package git

import (
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	gitfs "github.com/go-git/go-git/v5/storage/filesystem"
)

// localRepoLoader resolves a file:// endpoint path to a storer.Storer,
// supporting both bare repositories (objects/refs directly under the path)
// and standard repositories (objects/refs under a ".git" subdirectory).
//
// This replaces go-git's built-in file transport, which shells out to the
// git-upload-pack/git-receive-pack binaries. Using this pure-Go transport
// instead means doco-cd never needs a git binary to poll and deploy Git
// repositories that live on the local filesystem.
type localRepoLoader struct {
	base billy.Filesystem
}

// Load implements server.Loader.
func (l *localRepoLoader) Load(ep *transport.Endpoint) (storer.Storer, error) {
	fs, err := l.base.Chroot(ep.Path)
	if err != nil {
		return nil, err
	}

	if _, err = fs.Stat("config"); err != nil {
		// Not a bare repo layout (no "config" file at the root); fall back
		// to the ".git" subdirectory used by standard (non-bare) repos.
		if _, statErr := fs.Stat(".git"); statErr != nil {
			return nil, transport.ErrRepositoryNotFound
		}

		fs, err = fs.Chroot(".git")
		if err != nil {
			return nil, err
		}
	}

	return gitfs.NewStorage(fs, cache.NewObjectLRUDefault()), nil
}

// init registers a pure-Go, in-process implementation of the "file" transport
// scheme, overriding go-git's default subprocess-based implementation. This
// enables cloning/fetching from local filesystem Git repositories (file://
// URLs) without requiring a git binary on PATH.
func init() {
	loader := &localRepoLoader{base: osfs.New("/")}
	gitclient.InstallProtocol("file", server.NewClient(loader))
}
