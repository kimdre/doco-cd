package docker

import (
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/git"
)

// cloneTestRepoBranch clones a single branch of a test repository directly
// into dir using go-git, mirroring what the old git.CloneOrUpdateRepository
// test helper used to do for a fresh (non-existent) target directory. It
// exists so package tests don't need to shell out to git or depend on the
// production clone/store machinery (bare mirror + tree export), which is
// unnecessary for tests that only need a plain checked-out working tree.
func cloneTestRepoBranch(t *testing.T, dir, cloneURL, ref string, private bool, c *app.Config) *gogit.Repository {
	t.Helper()

	auth, err := git.GetAuthMethod(cloneURL, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("failed to get auth method: %v", err)
	}

	if auth == nil && private {
		t.Fatal("missing auth for private repository")
	}

	cloneOpts := &gogit.CloneOptions{
		URL:             cloneURL,
		Auth:            auth,
		ReferenceName:   plumbing.ReferenceName(ref),
		SingleBranch:    true,
		InsecureSkipTLS: c.SkipTLSVerification,
	}

	if c.HttpProxy != (transport.ProxyOptions{}) {
		cloneOpts.ProxyOptions = c.HttpProxy
	}

	if c.GitCloneSubmodules {
		cloneOpts.RecurseSubmodules = gogit.DefaultSubmoduleRecursionDepth
	}

	repo, err := gogit.PlainClone(dir, false, cloneOpts)
	if err != nil {
		t.Fatalf("failed to clone repository: %v", err)
	}

	return repo
}

// cloneTestRepoAllBranches clones every branch of a test repository into dir
// using go-git, for tests that need to check out arbitrary commits (not just
// the tip of one branch) via checkoutTestCommit afterwards.
func cloneTestRepoAllBranches(t *testing.T, dir, cloneURL string, auth transport.AuthMethod, c *app.Config) *gogit.Repository {
	t.Helper()

	cloneOpts := &gogit.CloneOptions{
		URL:             cloneURL,
		Auth:            auth,
		InsecureSkipTLS: c.SkipTLSVerification,
	}

	if c.HttpProxy != (transport.ProxyOptions{}) {
		cloneOpts.ProxyOptions = c.HttpProxy
	}

	if c.GitCloneSubmodules {
		cloneOpts.RecurseSubmodules = gogit.DefaultSubmoduleRecursionDepth
	}

	repo, err := gogit.PlainClone(dir, false, cloneOpts)
	if err != nil {
		t.Fatalf("failed to clone repository: %v", err)
	}

	return repo
}

// checkoutTestCommit checks out commitHash in repo's worktree.
func checkoutTestCommit(t *testing.T, repo *gogit.Repository, commitHash string) {
	t.Helper()

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if err := w.Checkout(&gogit.CheckoutOptions{
		Hash:  plumbing.NewHash(commitHash),
		Force: true,
	}); err != nil {
		t.Fatalf("failed to checkout commit %s: %v", commitHash, err)
	}
}
