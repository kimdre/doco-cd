// GitStore implements Store on top of a Git repository.
//
// Its mirror clone is bare: fetch-only, with no working tree and no HEAD to
// manage (git.CloneOrUpdateBareMirror). Publish exports that mirror's tree
// for the requested revision via git.ExportTree (handling regular files,
// symlinks and submodules), then decrypts every SOPS-encrypted file the
// export produced (decrypt.go) before the artifact is published.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/git"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

// GitStoreOptions configures a GitStore. It mirrors the parameters that
// git.CloneOrUpdateRepository already takes, so a GitStore can be
// constructed from the same values callers use today.
type GitStoreOptions struct {
	// Log receives debug/info output. Required.
	Log *slog.Logger
	// CloneURL is the repository to fetch from. Required.
	CloneURL string
	// BaseDir is the store's private directory: it holds the mirror clone
	// at "<BaseDir>/mirror" and published artifacts under
	// "<BaseDir>/artifacts/<revision>". Required.
	BaseDir string

	Private                 bool
	SSHPrivateKey           string
	SSHPrivateKeyPassphrase string
	AccessToken             string
	SkipTLSVerify           bool
	ProxyOptions            transport.ProxyOptions
	// CloneSubmodules enables submodule materialization in Publish, via
	// git.ExportTree's own fetch-and-recurse logic. It has no effect on
	// Resolve/the mirror clone itself: the mirror never needs its
	// submodules checked out, since Publish reads tree objects, not the
	// mirror's worktree.
	CloneSubmodules bool
	// Depth is the shallow-clone depth passed to the underlying sync.
	// 0 clones full history.
	Depth int
}

// GitStore is a Git-backed Store.
// See the package doc for its current bridging scope.
type GitStore struct {
	opts      GitStoreOptions
	mirrorDir string
}

var _ Store = (*GitStore)(nil)

// NewGitStore returns a GitStore configured by opts.
func NewGitStore(opts GitStoreOptions) (*GitStore, error) {
	if opts.CloneURL == "" {
		return nil, errors.New("git store: clone URL is required")
	}

	if opts.BaseDir == "" {
		return nil, errors.New("git store: base directory is required")
	}

	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	if err := sweepOrphanedTemp(opts.BaseDir); err != nil {
		opts.Log.Warn("failed to sweep orphaned artifact temp directories", slog.Any("error", err))
	}

	return &GitStore{
		opts:      opts,
		mirrorDir: filepath.Join(opts.BaseDir, MirrorSubdir),
	}, nil
}

// Resolve fetches ref into the store's mirror and returns the commit SHA it currently points to.
func (s *GitStore) Resolve(ctx context.Context, ref string) (Revision, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// CloneOrUpdateBareMirror already holds the mirror's path lock for the
	// full clone/fetch operation; acquiring it again here would deadlock
	// against ourselves. It never populates submodules either: Publish
	// materializes them itself, from tree objects, via git.ExportTree.
	if _, err := git.CloneOrUpdateBareMirror(s.opts.Log,
		s.opts.CloneURL, ref, s.mirrorDir,
		s.opts.Private, s.opts.SSHPrivateKey, s.opts.SSHPrivateKeyPassphrase, s.opts.AccessToken,
		s.opts.SkipTLSVerify, s.opts.ProxyOptions, s.opts.Depth); err != nil {
		return "", fmt.Errorf("resolve %q: %w", ref, err)
	}

	// CloneOrUpdateBareMirror releases the mirror's exclusive path lock
	// before returning, so a concurrent Resolve/Publish call for the same
	// mirror can start fetching (and briefly mutate refs) between that
	// release and this read. Re-open the repo under the shared lock via
	// git.MirrorRead so this read is fully ordered before or after any such
	// concurrent fetch, instead of racing it - reusing the returned repo
	// handle here reproduced exactly that race.
	sha, err := git.MirrorRead(s.mirrorDir, func(repo *gogit.Repository) (string, error) {
		return git.GetLatestCommit(repo, ref)
	})
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", ref, err)
	}

	return Revision(sha), nil
}

// Publish exports the mirror's tree at revision into a read-only artifact
// directory. revision must already be reachable in the mirror - normally
// guaranteed by a prior Resolve call for the reference that named it. If it
// is not reachable, Publish returns ErrRevisionNotFound rather than
// silently doing an unbounded fetch to find it.
func (s *GitStore) Publish(ctx context.Context, revision Revision) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}

	if existing, ok, err := s.Lookup(revision); err != nil {
		return Artifact{}, err
	} else if ok {
		return existing, nil
	}

	unlock := sourcecache.AcquireExclusivePathLock(s.mirrorDir)
	defer unlock()

	repo, err := git.OpenRepository(s.mirrorDir)
	if err != nil {
		return Artifact{}, fmt.Errorf("publish %s: %w", revision, err)
	}

	hash, err := git.ResolveReferenceCommit(repo, string(revision))
	if err != nil {
		return Artifact{}, fmt.Errorf("publish %s: %w: %w", revision, ErrRevisionNotFound, err)
	}

	exportOpts := git.ExportOptions{
		Log:                     s.opts.Log,
		Private:                 s.opts.Private,
		SSHPrivateKey:           s.opts.SSHPrivateKey,
		SSHPrivateKeyPassphrase: s.opts.SSHPrivateKeyPassphrase,
		AccessToken:             s.opts.AccessToken,
		SkipTLSVerify:           s.opts.SkipTLSVerify,
		ProxyOptions:            s.opts.ProxyOptions,
		Depth:                   s.opts.Depth,
	}

	if s.opts.CloneSubmodules {
		exportOpts.SubmoduleCacheDir = filepath.Join(s.opts.BaseDir, SubmodulesSubdir)
	}

	return publishDir(s.opts.BaseDir, revision, func(dir string) error {
		if err := git.ExportTree(dir, repo, hash, exportOpts); err != nil {
			return err
		}

		return decryptArtifact(s.opts.Log, dir)
	})
}

// Lookup returns the already-published artifact for revision, if any.
func (s *GitStore) Lookup(revision Revision) (Artifact, bool, error) {
	return lookupArtifact(s.opts.BaseDir, revision)
}

// MirrorDir returns the absolute path of the store's bare mirror clone.
// Read-only callers that need a *git.Repository handle for history queries
// (e.g. resolving a different reference, or GetLatestCommit/
// GetChangedFilesBetweenCommits-style lookups) can open it directly with
// git.OpenRepository. Such reads should still hold a shared
// sourcecache.AcquireSharedPathLock(mirrorDir) for their duration: Resolve/
// Publish only exclude each other via the matching exclusive lock, so an
// unguarded read can otherwise observe the mirror mid-fetch from a
// concurrent stack sharing this repository.
func (s *GitStore) MirrorDir() string {
	return s.mirrorDir
}

// List returns every artifact this store has published.
func (s *GitStore) List() ([]Artifact, error) {
	return listArtifacts(s.opts.BaseDir)
}
