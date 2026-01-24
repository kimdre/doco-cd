package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git/ssh"
)

const (
	DefaultShortSHALength = 7 // Default length for shortened commit SHAs
	RemoteName            = "origin"
	TagPrefix             = "refs/tags/"
	BranchPrefix          = "refs/heads/"
	MainBranch            = "refs/heads/main"
	SwarmModeBranch       = "refs/heads/swarm-mode"
	refSpecAllBranches    = "+refs/heads/*:refs/remotes/origin/*"
	refSpecSingleBranch   = "+refs/heads/%s:refs/remotes/origin/%s"
	refSpecAllTags        = "+refs/tags/*:refs/tags/*"
	refSpecSingleTag      = "+refs/tags/%s:refs/tags/%s"
)

var (
	ErrCheckoutFailed             = errors.New("failed to checkout repository")
	ErrFetchFailed                = errors.New("failed to fetch repository")
	ErrPullFailed                 = errors.New("failed to pull repository")
	ErrRepositoryAlreadyExists    = git.ErrRepositoryAlreadyExists
	ErrInvalidReference           = git.ErrInvalidReference
	ErrSSHKeyRequired             = errors.New("ssh URL requires SSH_PRIVATE_KEY to be set")
	ErrPossibleAuthMethodMismatch = errors.New("there might be a mismatch between the authentication method and the repository or submodule remote URL")
)

// ChangedFile represents a file that has changed between two commits.
type ChangedFile struct {
	// From represents the file state before the change.
	From diff.File
	// To represents the file state after the change.
	To diff.File
}

type RefSet struct {
	localRef  plumbing.ReferenceName
	remoteRef plumbing.ReferenceName
}

// GetReferenceSet retrieves a RefSet of local and remote references for a given reference name.
func GetReferenceSet(repo *git.Repository, ref string) (RefSet, error) {
	var refCandidates []RefSet

	// Check if the reference is a branch or tag
	switch {
	case strings.HasPrefix(ref, BranchPrefix):
		name := strings.TrimPrefix(ref, BranchPrefix)

		refCandidates = append(refCandidates, RefSet{
			localRef:  plumbing.NewBranchReferenceName(name),
			remoteRef: plumbing.NewRemoteReferenceName(RemoteName, name),
		})
	case strings.HasPrefix(ref, TagPrefix):
		name := strings.TrimPrefix(ref, TagPrefix)

		refCandidates = append(refCandidates, RefSet{
			localRef:  plumbing.NewTagReferenceName(name),
			remoteRef: plumbing.NewTagReferenceName(name),
		})
	default:
		// Create ref candidate for branch and tag
		refCandidates = append(refCandidates,
			RefSet{
				// Create ref candidate for branch
				localRef:  plumbing.NewBranchReferenceName(ref),
				remoteRef: plumbing.NewRemoteReferenceName(RemoteName, ref),
			},
			// Create ref candidate for tag
			RefSet{
				localRef:  plumbing.NewTagReferenceName(ref),
				remoteRef: plumbing.NewTagReferenceName(ref),
			},
		)
	}

	for _, candidate := range refCandidates {
		if candidate.localRef.IsBranch() {
			newRef := plumbing.NewSymbolicReference(candidate.localRef, candidate.remoteRef)

			err := repo.Storer.SetReference(newRef)
			if err != nil {
				return RefSet{}, err
			}
		}
		// Check if localRef exists remotely
		_, err := repo.Reference(candidate.remoteRef, true)
		if err != nil {
			if errors.Is(err, plumbing.ErrReferenceNotFound) {
				// If the reference does not exist, continue To the next candidate
				continue
			}

			return RefSet{}, fmt.Errorf("%w: %s", err, candidate.localRef)
		}

		return candidate, nil
	}

	return RefSet{}, fmt.Errorf("%w: %s", ErrInvalidReference, ref)
}

// IsSSH checks if a given URL is an SSH URL.
func IsSSH(url string) bool {
	return strings.HasPrefix(url, "git@") || strings.HasPrefix(url, "ssh://")
}

// SSHAuth creates an SSH authentication method using the provided private key.
func SSHAuth(privateKey, keyPassphrase string) (transport.AuthMethod, error) {
	if strings.TrimSpace(privateKey) == "" {
		return nil, ErrSSHKeyRequired
	}

	auth, err := gitssh.NewPublicKeys(ssh.DefaultGitSSHUser, []byte(privateKey), keyPassphrase)
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH public keys: %w", err)
	}

	// auth.HostKeyCallback = ssh2.InsecureIgnoreHostKey()

	return auth, nil
}

// HttpTokenAuth returns an AuthMethod for HTTP Basic Auth using a token.
func HttpTokenAuth(token string) transport.AuthMethod {
	if token == "" {
		return nil
	}

	return &githttp.BasicAuth{
		Username: "oauth2", // can be anything except an empty string
		Password: token,
	}
}

// addToKnownHosts adds the host from the SSH URL to the known_hosts file.
func addToKnownHosts(url string) error {
	err := ssh.CreateKnownHostsFile()
	if err != nil {
		return fmt.Errorf("failed to create known_hosts file: %w", err)
	}

	host, err := ssh.ExtractHostFromSSHUrl(url)
	if err != nil {
		return fmt.Errorf("failed to extract host from SSH URL: %w", err)
	}

	return ssh.AddHostToKnownHosts(host)
}

// convertSSHUrl converts SSH URLs to the ssh:// format.
// e.g. convert git@github.com:user/repo.git to ssh://git@github.com/user/repo.git
func convertSSHUrl(url string) string {
	// Check if url starts with git@ and convert to ssh:// format
	if strings.HasPrefix(url, "git@") {
		// Replace the first ':' with '/' after the host
		if idx := strings.Index(url, ":"); idx != -1 {
			url = url[:idx] + "/" + url[idx+1:]
		}

		url = "ssh://" + url
	}

	return url
}

// updateRemoteURL updates the remote URL of the repository.
func updateRemoteURL(repo *git.Repository, url string) error {
	// Update remote URL in case it has changed
	remote, err := repo.Remote(RemoteName)
	if err != nil {
		return fmt.Errorf("failed to get remote %s: %w", RemoteName, err)
	}

	c := remote.Config()

	var newUrl []string
	if IsSSH(url) {
		newUrl = []string{convertSSHUrl(url)}
	} else {
		newUrl = []string{url}
	}

	if slices.Compare(c.URLs, newUrl) == 0 {
		// No change in URL
		return nil
	}

	c.URLs = newUrl

	err = repo.DeleteRemote(RemoteName)
	if err != nil {
		return fmt.Errorf("failed to delete remote %s: %w", RemoteName, err)
	}

	_, err = repo.CreateRemote(c)
	if err != nil {
		return fmt.Errorf("failed to create remote %s: %w", RemoteName, err)
	}

	return nil
}

// UpdateRepository fetches and checks out the requested ref.
func UpdateRepository(path, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, cloneSubmodules bool) (*git.Repository, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return nil, err
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err
	}

	err = updateRemoteURL(repo, url)
	if err != nil {
		return nil, err
	}

	opts := &git.FetchOptions{
		RemoteName: RemoteName,
		RemoteURL:  url,
		RefSpecs:   []config.RefSpec{refSpecAllBranches, refSpecAllTags},
		Prune:      true,
		Auth:       auth,
	}

	// SSH auth when key is provided
	if IsSSH(url) {
		err = addToKnownHosts(url)
		if err != nil {
			return nil, fmt.Errorf("failed to add host to known_hosts: %w", err)
		}

		opts.RemoteURL = convertSSHUrl(url)
	} else {
		opts.InsecureSkipTLS = skipTLSVerify

		if proxyOpts != (transport.ProxyOptions{}) {
			opts.ProxyOptions = proxyOpts
		}
	}

	if err = repo.Fetch(opts); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, fmt.Errorf("%w: %w", ErrFetchFailed, err)
	}

	refSet, err := GetReferenceSet(repo, ref)
	if err != nil {
		return nil, err
	}

	if refSet.localRef == "" {
		return nil, fmt.Errorf("%w: %s", ErrInvalidReference, ref)
	}

	if err = worktree.Checkout(&git.CheckoutOptions{Branch: refSet.localRef, Keep: true}); err != nil {
		return nil, fmt.Errorf("%w: %w: %s", ErrCheckoutFailed, err, refSet.localRef)
	}

	if err = ResetTrackedFiles(repo); err != nil {
		return nil, fmt.Errorf("failed to reset tracked files: %w", err)
	}

	if cloneSubmodules {
		if err = updateSubmodules(repo, opts.Auth); err != nil {
			return nil, fmt.Errorf("failed to update submodules: %w", err)
		}
	}

	return repo, nil
}

// CloneRepository clones a repository with HTTP or SSH auth.
func CloneRepository(path, url, ref string, skipTLSVerify bool, proxyOpts transport.ProxyOptions, auth transport.AuthMethod, cloneSubmodules bool) (*git.Repository, error) {
	err := os.MkdirAll(path, filesystem.PermDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", path, err)
	}

	opts := &git.CloneOptions{
		RemoteName:    RemoteName,
		URL:           url,
		SingleBranch:  true,
		ReferenceName: plumbing.ReferenceName(ref),
		Tags:          git.NoTags,
		Auth:          auth,
	}

	if cloneSubmodules {
		opts.RecurseSubmodules = git.DefaultSubmoduleRecursionDepth
	}

	if IsSSH(url) {
		err = addToKnownHosts(url)
		if err != nil {
			return nil, fmt.Errorf("failed to add host to known_hosts: %w", err)
		}

		opts.URL = convertSSHUrl(url)
	} else {
		opts.InsecureSkipTLS = skipTLSVerify

		if proxyOpts != (transport.ProxyOptions{}) {
			opts.ProxyOptions = proxyOpts
		}
	}

	repo, err := git.PlainClone(path, false, opts)
	if errors.Is(err, transport.ErrInvalidAuthMethod) && cloneSubmodules {
		return nil, fmt.Errorf("%w: %w", err, ErrPossibleAuthMethodMismatch)
	}

	return repo, err
}

func updateSubmodules(repo *git.Repository, auth transport.AuthMethod) error {
	worktree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	submodules, err := worktree.Submodules()
	if err != nil {
		return fmt.Errorf("failed to list submodules: %w", err)
	}

	for _, submodule := range submodules {
		submoduleRepo, err := submodule.Repository()
		if err != nil {
			return fmt.Errorf("failed to get submodule repository: %w", err)
		}

		// Reset tracked files in submodule
		err = ResetTrackedFiles(submoduleRepo)
		if err != nil {
			return fmt.Errorf("failed to reset tracked files in submodule: %w", err)
		}

		opts := &git.SubmoduleUpdateOptions{
			Init:              true,
			RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
			Auth:              auth,
		}

		if err = submodule.Update(opts); err != nil {
			submodulePath := "submodule"
			if cfg := submodule.Config(); cfg.Path != "" {
				submodulePath = cfg.Path
			}

			if errors.Is(err, git.ErrUnstagedChanges) {
				// Hard reset and try again
				submoduleRepoWorktree, err := submoduleRepo.Worktree()
				if err != nil {
					return fmt.Errorf("failed to get worktree for %s: %w", submodulePath, err)
				}

				err = submoduleRepoWorktree.Reset(&git.ResetOptions{
					Mode: git.HardReset,
				})
				if err != nil {
					return fmt.Errorf("failed to reset worktree for %s: %w", submodulePath, err)
				}

				// Retry submodule update
				err = submodule.Update(opts)
				if err != nil {
					return fmt.Errorf("failed to update %s after resetting: %w", submodulePath, err)
				}

				continue
			} else if errors.Is(err, transport.ErrInvalidAuthMethod) {
				return fmt.Errorf("%w: %w", err, ErrPossibleAuthMethodMismatch)
			}

			return fmt.Errorf("failed to update %s: %w", submodulePath, err)
		}
	}

	return nil
}

// GetAuthUrl returns a clone URL with an access token for private repositories.
func GetAuthUrl(url, authType, token string) string {
	// Retrieve the protocol From the clone URL (e.g. https://, http://, git://
	protocol := regexp.MustCompile("^(https?|git)://").FindString(url)

	return protocol + authType + ":" + token + "@" + url[len(protocol):]
}

// GetLatestCommit retrieves the last commit hash for a given reference in a repository.
func GetLatestCommit(repo *git.Repository, ref string) (string, error) {
	// Get the reference for the specified ref
	refSet, err := GetReferenceSet(repo, ref)
	if err != nil {
		return plumbing.ZeroHash.String(), err
	}

	r, err := repo.Reference(refSet.remoteRef, true)
	if err != nil {
		return plumbing.ZeroHash.String(), fmt.Errorf("failed to get reference %s: %w", ref, err)
	}

	// Get the commit object for the reference
	commit, err := repo.CommitObject(r.Hash())
	if err != nil {
		return plumbing.ZeroHash.String(), fmt.Errorf("failed to get commit object for %s: %w", r.Hash(), err)
	}

	return commit.Hash.String(), nil
}

// ResetTrackedFiles resets all tracked files in the worktree To their last committed state
// while leaving untracked files intact.
func ResetTrackedFiles(repo *git.Repository) error {
	worktree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	repoRoot := worktree.Filesystem.Root()

	changedFiles, err := worktree.Status()
	if err != nil {
		return fmt.Errorf("failed to get worktree status: %w", err)
	}

	resetFiles := make([]string, 0, len(changedFiles))

	for file, status := range changedFiles {
		// Do not touch files that are not part of the Git repository (e.g. created by a container process)
		if status.Staging == git.Untracked {
			continue
		}

		if shouldResetDecryptedFile(repo, repoRoot, file) {
			resetFiles = append(resetFiles, file)
		}
	}

	if len(resetFiles) > 0 {
		err = worktree.Reset(&git.ResetOptions{
			Mode:  git.HardReset,
			Files: resetFiles,
		})
		if err != nil {
			return fmt.Errorf("failed to reset worktree: %w", err)
		}
	}

	return nil
}

// GetChangedFilesBetweenCommits retrieves a list of changed files between two commits in a repository.
func GetChangedFilesBetweenCommits(repo *git.Repository, commitHash1, commitHash2 plumbing.Hash) ([]ChangedFile, error) {
	commit1, err := repo.CommitObject(commitHash1)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit From commitHash1 %s: %w", commitHash1, err)
	}

	commit2, err := repo.CommitObject(commitHash2)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit From commitHash2 %s: %w", commitHash2, err)
	}

	// Create a patch between the two commits
	patch, err := commit1.Patch(commit2)
	if err != nil {
		return nil, fmt.Errorf("failed to create patch: %w", err)
	}

	changedFiles := make([]ChangedFile, 0, len(patch.FilePatches()))
	for _, file := range patch.FilePatches() {
		from, to := file.Files()
		changedFiles = append(changedFiles, ChangedFile{From: from, To: to})
	}

	return changedFiles, nil
}

// HasChangesInSubdir checks if any of the changed files are in a specified subdirectory.
func HasChangesInSubdir(changedFiles []ChangedFile, workingDir, subdir string) (bool, error) {
	// Collect all symlinks in subdir
	symlinks := make(map[string]string)

	err := filepath.Walk(filepath.Join(workingDir, subdir), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("failed to walk subdir %s: %w", subdir, err)
		}

		// Check if the file is a symlink
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("failed to read symlink: %w", err)
			}

			absTarget := target
			if !filepath.IsAbs(target) {
				absTarget = filepath.Join(filepath.Dir(path), target)
			}

			symlinks[path] = absTarget
		}

		return nil
	})
	if err != nil {
		return false, err
	}

	for _, file := range changedFiles {
		var paths []string

		if file.From != nil {
			paths = append(paths, file.From.Path())
		}

		if file.To != nil {
			paths = append(paths, file.To.Path())
		}

		for _, p := range paths {
			rel, err := filepath.Rel(subdir, p)

			if err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
				return true, nil
			}

			// Check if file is inside any symlink target
			for _, target := range symlinks {
				relSymlink, err := filepath.Rel(target, filepath.Join(target, p))
				if err != nil {
					continue
				}

				if relSymlink == "." || !strings.HasPrefix(relSymlink, "..") {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// shouldResetDecryptedFile determines whether a file should be reset based on its decrypted content.
func shouldResetDecryptedFile(repo *git.Repository, repoRoot, file string) bool {
	headRef, err := repo.Head()
	if err != nil {
		return true
	}

	commit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return true
	}
	// Get file from commit tree
	fileObj, err := commit.File(file)
	if err != nil {
		return true // Not tracked, default to reset
	}

	committedBytes, err := fileObj.Contents()
	if err != nil {
		return true
	}

	format := encryption.GetFileFormat(fileObj.Name)

	decryptedContent, err := encryption.DecryptContent([]byte(committedBytes), format)
	if err != nil {
		return true
	}

	workingContent, err := os.ReadFile(filepath.Join(repoRoot, file)) // #nosec G304
	if err != nil {
		return true
	}

	return !strings.EqualFold(string(decryptedContent), string(workingContent))
}

// GetShortestUniqueCommitSHA returns the shortest unique prefix of a commit SHA in the repository.
// Similar to the git command `git rev-parse --short=<length> <commitSHA>`.
func GetShortestUniqueCommitSHA(repo *git.Repository, commitSHA string, minLength int) (string, error) {
	iter, err := repo.CommitObjects()
	if err != nil {
		return "", err
	}
	defer iter.Close()

	// collect all commit SHAs
	var allSHAs []string

	err = iter.ForEach(func(c *object.Commit) error {
		allSHAs = append(allSHAs, c.Hash.String())
		return nil
	})
	if err != nil {
		return "", err
	}

	shaLen := len(commitSHA)
	for length := minLength; length <= shaLen; length++ {
		prefixCount := make(map[string]int, len(allSHAs))
		for _, sha := range allSHAs {
			if len(sha) >= length {
				prefix := sha[:length]
				prefixCount[prefix]++
			}
		}

		prefix := commitSHA[:length]
		if prefixCount[prefix] == 1 {
			return prefix, nil
		}
	}

	return "", fmt.Errorf("no unique prefix found for commit SHA %s", commitSHA)
}
