package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	gitInternal "github.com/kimdre/doco-cd/internal/git"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// AutoDiscoveryOrigin describes the auto-discovery config that discovered a deployment config.
type AutoDiscoveryOrigin struct {
	// WorkingDirectory is the directory the auto-discovery config scans, relative to the source root.
	WorkingDirectory string
	// Reference is the reference the auto-discovery config scans.
	Reference string
	// RepositoryURL is the repository the auto-discovery config scans. Empty means the job's own source.
	RepositoryURL string
	// WebhookEventFilter is the webhook event filter of the auto-discovery config. Nested configs can
	// override the filter of a discovered deployment, so it is kept separately.
	WebhookEventFilter string
	// Revision is the Git revision the deployment was discovered in. Empty if unknown.
	Revision string
	// MirrorDir is the Git mirror Revision was resolved in. Empty if Revision is.
	MirrorDir string
	// Settings are the auto-discovery settings of the config.
	Settings AutoDiscoveryConfig
}

// Owns reports whether relDir, a stack's working directory relative to the source root, lies within
// the directories o scans.
func (o *AutoDiscoveryOrigin) Owns(relDir string) bool {
	if o == nil {
		return false
	}

	root := path.Clean(o.WorkingDirectory)
	below := path.Clean(relDir)

	if root != "." {
		if below == root {
			return true
		}

		var ok bool
		if below, ok = strings.CutPrefix(below, root+"/"); !ok {
			return false
		}
	}

	if below == "." {
		return true
	}

	if below == ".." || strings.HasPrefix(below, "../") || path.IsAbs(below) {
		return false
	}

	return o.Settings.ScanDepth == 0 || strings.Count(below, "/")+1 <= o.Settings.ScanDepth
}

// acquireMirrorReadLock guards read-only access to a shared bare mirror's object database and refs.
// GitStore.Resolve/Publish only exclude each other via the matching exclusive lock, so an unguarded read
// can observe the mirror mid-fetch while another deployment of the same repository runs concurrently.
// The lock is not reentrant, so it must never be held across a call that resolves or publishes into the same mirror.
func acquireMirrorReadLock(mirrorDir string) func() {
	if mirrorDir == "" {
		return func() {}
	}

	return gitInternal.AcquireSharedMirrorLock(mirrorDir)
}

// discoverySource is the source a job's auto-discovery configs scan.
type discoverySource struct {
	// repoRoot is the published artifact of the job's revision, or a plain directory for other sources.
	repoRoot string
	// labelRoot is a revision-stable directory naming the repository, see repositoryLabelRoot.
	labelRoot string
	// mirrorRoot is the job's Git mirror. Empty for non-Git sources and Git checkouts.
	mirrorRoot string
	// gitRoot is the repository references are resolved in: mirrorRoot, or repoRoot.
	gitRoot string
	isGit   bool
	// reference and revision identify what the job resolved. revision is empty if unknown.
	reference string
	revision  string
	opts      *GitOptions
}

// newDiscoverySource returns the discovery source for a job that resolved reference to primaryRevision,
// published in repoRoot. mirrorRoot and primaryRevision are empty for non-Git sources.
func newDiscoverySource(repoRoot, reference, mirrorRoot, primaryRevision string, opts *GitOptions) (*discoverySource, error) {
	if opts == nil {
		opts = &GitOptions{}
	}

	s := &discoverySource{
		repoRoot:   repoRoot,
		labelRoot:  repositoryLabelRoot(repoRoot, mirrorRoot),
		mirrorRoot: mirrorRoot,
		gitRoot:    repoRoot,
		reference:  reference,
		revision:   primaryRevision,
		opts:       opts,
	}

	if mirrorRoot != "" {
		s.gitRoot = mirrorRoot
	}

	unlock := acquireMirrorReadLock(mirrorRoot)
	_, err := git.PlainOpen(s.gitRoot)

	unlock()

	switch {
	case err == nil:
		s.isGit = true
	case errors.Is(err, git.ErrRepositoryNotExists):
	default:
		return nil, fmt.Errorf("failed to open git repository at %s: %w", s.gitRoot, err)
	}

	return s, nil
}

// discover returns the deployment configs c discovers.
func (s *discoverySource) discover(ctx context.Context, c *Config) ([]*Config, error) {
	if c.AutoDiscovery.ScanDepth < 0 {
		return nil, fmt.Errorf("%w: auto_discovery.depth must be >= 0", ErrInvalidConfig)
	}

	switch {
	case c.RepositoryUrl != "":
		return s.discoverRemote(ctx, c)
	case s.isGit:
		return s.discoverGit(ctx, c)
	default:
		configs, err := autoDiscoverDeployments(os.DirFS(s.repoRoot), s.labelRoot, "", c)

		return scanDiscovery("", configs, err)
	}
}

// discoverGit discovers c in the job's own Git repository.
func (s *discoverySource) discoverGit(ctx context.Context, c *Config) ([]*Config, error) {
	// The job already resolved its reference. Resolving it again could see a newer
	// mirror tip than the revision the job deploys, and remove stacks it still has.
	if s.revision != "" && gitInternal.ReferenceMatches(s.reference, c.Reference) {
		return s.scanPrimary(c, plumbing.NewHash(s.revision))
	}

	if s.mirrorRoot != "" && s.opts.SourceURL != "" {
		baseDir := filepath.Dir(s.mirrorRoot)

		gitStore, err := s.newStore(s.opts.SourceURL, baseDir, c)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize git store: %w", err)
		}

		return s.publishAndScan(ctx, gitStore, baseDir, s.labelRoot, c)
	}

	// Without a store, the reference can only be read from the objects that are already there.
	return s.discoverMirrorTree(c)
}

// discoverRemote discovers c in the repository named by its repository_url.
func (s *discoverySource) discoverRemote(ctx context.Context, c *Config) ([]*Config, error) {
	storeDir := remoteDiscoveryStoreDir(s.opts.SourceBaseDir, s.repoRoot, string(c.RepositoryUrl))

	gitStore, err := s.newStore(string(c.RepositoryUrl), storeDir, c)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize git store for %s: %w", c.RepositoryUrl, err)
	}

	return s.publishAndScan(ctx, gitStore, storeDir, storeDir, c)
}

// newStore returns a store for cloneURL in baseDir with the settings deployments of c use, so its
// artifacts are shared with them.
func (s *discoverySource) newStore(cloneURL, baseDir string, c *Config) (*store.GitStore, error) {
	return store.NewGitStore(store.GitStoreOptions{
		Log:                     slog.Default(),
		CloneURL:                cloneURL,
		BaseDir:                 baseDir,
		SSHPrivateKey:           s.opts.SSHPrivateKey,
		SSHPrivateKeyPassphrase: s.opts.SSHPrivateKeyPassphrase,
		AccessToken:             s.opts.GitAccessToken,
		SkipTLSVerify:           s.opts.SkipTLSVerification,
		ProxyOptions:            s.opts.HttpProxy,
		CloneSubmodules:         s.opts.GitCloneSubmodules,
		Depth:                   c.ResolveGitDepth(s.opts.GitCloneDepth),
	})
}

// publishAndScan fetches c's reference into gitStore, publishes its revision and scans the artifact.
// Nested configs are read from the artifact, so they are decrypted and their symlinks resolved like
// the files a deployment uses.
func (s *discoverySource) publishAndScan(ctx context.Context, gitStore *store.GitStore, storeDir, labelRoot string, c *Config) ([]*Config, error) {
	// Keeps the garbage collector from removing the artifact or the whole store during the scan.
	unlockGC, err := sourcecache.AcquireSharedGCPathLock(storeDir)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire artifact GC lock: %w", err)
	}
	defer unlockGC()

	revision, err := gitStore.Resolve(ctx, c.Reference)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve reference %s: %w", c.Reference, err)
	}

	hash := plumbing.NewHash(string(revision))

	if gitStore.MirrorDir() == s.mirrorRoot && string(revision) == s.revision {
		return s.scanPrimary(c, hash)
	}

	artifact, err := gitStore.Publish(ctx, revision)
	if err != nil {
		return nil, fmt.Errorf("failed to publish reference %s: %w", c.Reference, err)
	}

	return scanPublished(artifact.Path, labelRoot, gitStore.MirrorDir(), hash, c)
}

// discoverMirrorTree discovers c from the Git objects of its reference without fetching or publishing it.
// Nested configs are read as stored in Git, so encrypted or symlinked configs are not supported here.
func (s *discoverySource) discoverMirrorTree(c *Config) ([]*Config, error) {
	unlock := acquireMirrorReadLock(s.mirrorRoot)

	// A fresh handle per read region: go-git caches a handle's packfile index map on
	// first use, so a handle reused across another deployment's fetch can crash.
	repo, err := git.PlainOpen(s.gitRoot)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("failed to open git repository at %s: %w", s.gitRoot, err)
	}

	hash, err := gitInternal.ResolveReferenceCommit(repo, c.Reference)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("failed to resolve reference %s: %w", c.Reference, err)
	}

	matchesPrimary, err := matchesPrimaryContent(repo, hash, s.revision)
	if err != nil {
		unlock()
		return nil, err
	}

	if matchesPrimary {
		// publishedGitDiscoveryFS takes the same non-reentrant lock.
		unlock()

		return s.scanPrimary(c, hash)
	}

	defer unlock()

	// TreeFS reads objects lazily, so the mirror lock is held until the scan finishes.
	treeFS, err := gitInternal.NewTreeFSAtCommit(repo, hash)
	if err != nil {
		return nil, fmt.Errorf("failed to open tree for reference %s: %w", c.Reference, err)
	}

	configs, err := autoDiscoverDeployments(treeFS, s.labelRoot, hash.String(), c)

	return scanDiscovery(s.mirrorRoot, configs, err)
}

// scanPrimary scans the job's own artifact, which holds revision.
func (s *discoverySource) scanPrimary(c *Config, revision plumbing.Hash) ([]*Config, error) {
	return scanPublished(s.repoRoot, s.labelRoot, s.mirrorRoot, revision, c)
}

// scanPublished scans the published artifact root of revision.
func scanPublished(root, labelRoot, mirrorRoot string, revision plumbing.Hash, c *Config) ([]*Config, error) {
	fsys, release := publishedGitDiscoveryFS(root, labelRoot, mirrorRoot, revision, c)
	if release != nil {
		defer release()
	}

	configs, err := autoDiscoverDeployments(fsys, labelRoot, revision.String(), c)

	return scanDiscovery(mirrorRoot, configs, err)
}

// scanDiscovery records the mirror the configs were discovered from and wraps a scan error.
func scanDiscovery(mirrorDir string, configs []*Config, err error) ([]*Config, error) {
	if err != nil {
		return nil, fmt.Errorf("failed to auto-discover deployment configurations: %w", err)
	}

	for _, c := range configs {
		if origin := c.Internal.AutoDiscoveryOrigin; origin != nil && origin.Revision != "" {
			origin.MirrorDir = mirrorDir
		}
	}

	return configs, nil
}
