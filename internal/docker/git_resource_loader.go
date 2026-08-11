package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/remote"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/moby/buildkit/frontend/dockerfile/dfgitutil"

	"github.com/kimdre/doco-cd/internal/config/app"

	"github.com/kimdre/doco-cd/internal/git"
)

const gitIncludeCacheDirectory = "compose-git-cache"

var gitIncludeLocks sync.Map

type gitResourceLoader struct {
	cacheDirectory  string
	skipTLSVerify   bool
	proxyOptions    transport.ProxyOptions
	cloneSubmodules bool
	cloneDepth      int
	privateKey      string
	keyPassphrase   string
	accessToken     string
	known           map[string]string
	knownMu         sync.RWMutex
}

// gitResourceLoaderConfig configures a gitResourceLoader.
type gitResourceLoaderConfig struct {
	CacheBase       string
	SkipTLSVerify   bool
	ProxyOptions    transport.ProxyOptions
	CloneSubmodules bool
	CloneDepth      int
	PrivateKey      string
	KeyPassphrase   string
	AccessToken     string
}

// newRemoteResourceLoaders configures Git includes independently of the Docker CLI.
// OCI includes require the Docker CLI for registry credentials.
func newRemoteResourceLoaders(c *app.Config, dockerCli command.Cli) []loader.ResourceLoader {
	remoteLoaders := []loader.ResourceLoader{
		newGitResourceLoader(gitResourceLoaderConfig{
			CacheBase:       c.DataMountPath,
			SkipTLSVerify:   c.SkipTLSVerification,
			ProxyOptions:    c.HttpProxy,
			CloneSubmodules: c.GitCloneSubmodules,
			CloneDepth:      c.GitCloneDepth,
			PrivateKey:      c.SSHPrivateKey,
			KeyPassphrase:   c.SSHPrivateKeyPassphrase,
			AccessToken:     c.GitAccessToken,
		}),
	}
	if dockerCli != nil {
		remoteLoaders = append(remoteLoaders, remote.NewOCIRemoteLoader(dockerCli, false, api.OCIOptions{
			InsecureRegistries: c.OciInsecureRegistries,
		}))
	}

	return remoteLoaders
}

// newGitResourceLoader creates a gitResourceLoader with the given configuration.
func newGitResourceLoader(cfg gitResourceLoaderConfig) *gitResourceLoader {
	cacheBase := cfg.CacheBase
	if cacheBase == "" {
		cacheBase = os.TempDir()
	}

	return &gitResourceLoader{
		cacheDirectory:  filepath.Join(cacheBase, gitIncludeCacheDirectory),
		skipTLSVerify:   cfg.SkipTLSVerify,
		proxyOptions:    cfg.ProxyOptions,
		cloneSubmodules: cfg.CloneSubmodules,
		cloneDepth:      cfg.CloneDepth,
		privateKey:      cfg.PrivateKey,
		keyPassphrase:   cfg.KeyPassphrase,
		accessToken:     cfg.AccessToken,
		known:           map[string]string{},
	}
}

// Accept returns true if the resource is a valid Git reference.
func (g *gitResourceLoader) Accept(resource string) bool {
	_, _, err := dfgitutil.ParseGitRef(resource)

	return err == nil
}

// Load clones or updates the Git repository and returns the path to the included file.
func (g *gitResourceLoader) Load(ctx context.Context, resource string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	ref, _, err := dfgitutil.ParseGitRef(resource)
	if err != nil {
		return "", fmt.Errorf("parse git include %q: %w", resource, err)
	}

	if ref.Ref == "" {
		ref.Ref = "HEAD" // default branch
	}

	if err := os.MkdirAll(g.cacheDirectory, 0o700); err != nil {
		return "", fmt.Errorf("create git include cache: %w", err)
	}

	if err := os.Chmod(g.cacheDirectory, 0o700); err != nil {
		return "", fmt.Errorf("secure git include cache: %w", err)
	}

	// The cache is keyed by remote and ref so that concurrently loaded includes
	// of the same repository never switch the checkout under each other.
	repoPath := filepath.Join(g.cacheDirectory, cacheKey(ref.Remote, ref.Ref))
	lock, _ := gitIncludeLocks.LoadOrStore(repoPath, &sync.Mutex{})
	repoLock := lock.(*sync.Mutex)
	repoLock.Lock()
	err = g.checkout(repoPath, ref.Remote, ref.Ref)
	repoLock.Unlock()

	if err != nil {
		return "", err
	}

	localPath, err := gitIncludePath(repoPath, ref.SubDir)
	if err != nil {
		return "", err
	}

	if info, err := os.Stat(localPath); err != nil {
		return "", err
	} else if info.IsDir() {
		localPath, err = findComposeFile(localPath)
		if err != nil {
			return "", err
		}

		localPath, err = validateGitIncludePath(repoPath, localPath, localPath)
		if err != nil {
			return "", err
		}
	}

	directory := filepath.Dir(localPath)

	g.knownMu.Lock()
	// Compose calls Dir with the local path returned by Load as well as with the
	// original resource, so both have to resolve to the include's directory.
	g.known[resource] = directory
	g.known[localPath] = directory
	g.knownMu.Unlock()

	return localPath, nil
}

// Dir returns the directory of the given resource, which may be a Git reference or a local path.
func (g *gitResourceLoader) Dir(resource string) string {
	g.knownMu.RLock()
	directory, ok := g.known[resource]
	g.knownMu.RUnlock()

	if ok {
		return directory
	}

	if info, err := os.Stat(resource); err == nil {
		if info.IsDir() {
			return resource
		}

		return filepath.Dir(resource)
	}

	return ""
}

// checkout clones or updates the Git repository at the given path.
func (g *gitResourceLoader) checkout(path, remote, ref string) error {
	auth, err := git.GetAuthMethod(remote, g.privateKey, g.keyPassphrase, g.accessToken)
	if err != nil {
		return fmt.Errorf("authenticate git include %q: %w", remote, err)
	}

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		_, err = git.CloneRepository(path, remote, ref, g.skipTLSVerify, g.proxyOptions, auth, g.cloneSubmodules, g.cloneDepth)
		if err != nil {
			return fmt.Errorf("clone git include %q: %w", remote, err)
		}

		return nil
	} else if err != nil {
		return fmt.Errorf("stat git include cache: %w", err)
	}

	_, err = git.UpdateRepository(path, remote, ref, g.skipTLSVerify, g.proxyOptions, auth, g.cloneSubmodules, g.cloneDepth)
	if err != nil {
		return fmt.Errorf("update git include %q: %w", remote, err)
	}

	return nil
}

// cacheKey returns a unique key for the given remote and ref, suitable for use as a directory name.
func cacheKey(remote, ref string) string {
	sum := sha256.Sum256([]byte(remote + "\x00" + ref))

	return hex.EncodeToString(sum[:])
}

// gitIncludePath returns the absolute path to the included file or directory within the Git repository.
// It ensures that the path does not escape the repository.
func gitIncludePath(repoPath, subdir string) (string, error) {
	target := repoPath

	if subdir != "" {
		cleanSubdir := filepath.Clean(subdir)
		if filepath.IsAbs(cleanSubdir) || cleanSubdir == ".." ||
			strings.HasPrefix(cleanSubdir, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("git include subdirectory escapes repository: %q", subdir)
		}

		target = filepath.Join(repoPath, cleanSubdir)
	}

	return validateGitIncludePath(repoPath, target, subdir)
}

// validateGitIncludePath ensures that the target path is within the repository path.
func validateGitIncludePath(repoPath, target, description string) (string, error) {
	resolvedRepo, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		return "", err
	}

	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}

	relative, err := filepath.Rel(resolvedRepo, resolvedTarget)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("git include path escapes repository: %q", description)
	}

	return resolvedTarget, nil
}

// findComposeFile searches for a Docker Compose file in the given directory.
func findComposeFile(directory string) (string, error) {
	for _, name := range cli.DefaultFileNames {
		candidate := filepath.Join(directory, name)

		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}

		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}

	return "", fmt.Errorf("no compose file found in git include directory %q", directory)
}
