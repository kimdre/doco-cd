package git_test

import (
	"errors"
	"os"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/git"
)

func TestCloneRepository(t *testing.T) {
	t.Parallel()

	c, err := app.GetConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	testCases := []struct {
		name       string
		cloneUrl   string
		reference  string
		privateKey string
		passphrase string
		skip       bool
	}{
		{
			name:       "HTTP clone branch ref",
			cloneUrl:   cloneUrl,
			reference:  git.MainBranch,
			privateKey: "",
			passphrase: "",
			skip:       false,
		},
		{
			name:       "HTTP clone tag ref",
			cloneUrl:   cloneUrl,
			reference:  tagRef,
			privateKey: "",
			passphrase: "",
			skip:       false,
		},
		{
			name:       "HTTP clone sha ref",
			cloneUrl:   cloneUrl,
			reference:  commitSHARef,
			privateKey: "",
			passphrase: "",
			skip:       false,
		},
		{
			name:       "SSH clone",
			cloneUrl:   cloneUrlSSH,
			reference:  git.MainBranch,
			privateKey: c.SSHPrivateKey,
			passphrase: c.SSHPrivateKeyPassphrase,
			skip:       c.SSHPrivateKey == "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if tc.skip {
				t.Skip("SSH private key not set, skipping SSH clone test")
			}

			auth, err := git.GetAuthMethod(tc.cloneUrl, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
			if err != nil {
				t.Fatalf("Failed to get auth method: %v", err)
			}

			if auth != nil {
				t.Logf("Using auth method: %s", auth.Name())
			} else {
				t.Log("No auth method configured, using anonymous access")
			}

			repo, err := git.CloneRepository(t.TempDir(), tc.cloneUrl, tc.reference, false, c.HttpProxy, auth, c.GitCloneSubmodules, 0)
			if err != nil {
				t.Fatalf("Failed to clone repository: %v", err)
			}

			if repo == nil {
				t.Fatal("Repository is nil")
			}

			worktree, err := repo.Worktree()
			if err != nil {
				t.Fatalf("Failed to get worktree: %v", err)
			}

			t.Cleanup(func() {
				err = os.RemoveAll(worktree.Filesystem.Root())
				if err != nil {
					t.Fatalf("Failed to remove repository: %v", err)
				}
			})

			files, err := worktree.Filesystem.ReadDir(".")
			if err != nil {
				t.Fatalf("Failed to read directory: %v", err)
			}

			if len(files) == 0 {
				t.Fatal("No files in repository")
			}

			if worktree.Filesystem.Root() == "" {
				t.Fatal("Repository is not cloned")
			}
		})
	}
}

func TestUpdateRepository(t *testing.T) {
	testCases := []struct {
		name        string
		cloneUrl    string
		privateRepo bool
		branchRef   string
		expectedRef string
		expectedErr error
	}{
		{
			name:        "Valid branch ref",
			cloneUrl:    cloneUrl,
			privateRepo: false,
			branchRef:   git.MainBranch,
			expectedRef: remoteMainBranch,
			expectedErr: nil,
		},
		{
			name:        "Valid short branch ref",
			cloneUrl:    cloneUrl,
			branchRef:   "main",
			expectedRef: remoteMainBranch,
			expectedErr: nil,
		},
		{
			name:        "Valid tag ref",
			cloneUrl:    cloneUrl,
			privateRepo: false,
			branchRef:   remoteTagRef,
			expectedRef: remoteTagRef,
			expectedErr: nil,
		},
		{
			name:        "Valid short tag ref",
			cloneUrl:    cloneUrl,
			privateRepo: false,
			branchRef:   tagRef,
			expectedRef: remoteTagRef,
			expectedErr: nil,
		},
		{
			name:        "Valid commit SHA ref",
			cloneUrl:    cloneUrl,
			privateRepo: false,
			branchRef:   commitSHARef,
			expectedRef: commitSHARef,
			expectedErr: nil,
		},
		{
			name:        "Invalid branch ref",
			cloneUrl:    cloneUrl,
			privateRepo: false,
			branchRef:   invalidRef,
			expectedRef: "",
			expectedErr: git.ErrInvalidReference,
		},
		{
			name:        "Invalid tag ref",
			cloneUrl:    cloneUrl,
			privateRepo: false,
			branchRef:   invalidTagRef,
			expectedRef: "",
			expectedErr: git.ErrInvalidReference,
		},
		{
			name:        "Private Repository",
			cloneUrl:    cloneUrlTest,
			privateRepo: true,
			branchRef:   "destroy",
			expectedRef: "refs/remotes/origin/destroy",
			expectedErr: nil,
		},
	}

	encryption.SetupAgeKeyEnvVar(t)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, err := app.GetConfig()
			if err != nil {
				t.Fatalf("Failed to get app config: %v", err)
			}

			auth, err := git.GetAuthMethod(tc.cloneUrl, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
			if err != nil {
				t.Fatalf("Failed to get auth method: %v", err)
			}

			if auth != nil {
				t.Logf("Using auth method: %s", auth.Name())
			} else {
				t.Log("No auth method configured, using anonymous access")
			}

			repo, err := git.CloneRepository(t.TempDir(), tc.cloneUrl, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules, 0)
			if err != nil {
				t.Fatalf("Failed to clone repository %s: %v", tc.cloneUrl, err)
			}

			if repo == nil {
				t.Fatal("Repository is nil")
			}

			worktree, err := repo.Worktree()
			if err != nil {
				t.Fatalf("Failed to get worktree: %v", err)
			}

			repo, err = git.UpdateRepository(worktree.Filesystem.Root(), tc.cloneUrl, tc.branchRef, false, c.HttpProxy, auth, c.GitCloneSubmodules, 0)
			if err != nil {
				if !errors.Is(err, tc.expectedErr) {
					t.Fatalf("Expected error %v, got %v", tc.expectedErr, err)
				}

				return
			}

			if repo == nil && tc.expectedErr == nil {
				t.Fatal("Repository is nil")
			}

			if repo != nil {
				_, err = repo.Worktree()
				if err != nil {
					t.Fatalf("Failed to get worktree: %v", err)
				}
			}

			if plumbing.IsHash(tc.expectedRef) {
				commit, err := repo.CommitObject(plumbing.NewHash(tc.expectedRef))
				if err != nil {
					t.Fatalf("Failed to get commit object for %s: %v", tc.expectedRef, err)
				}

				if commit.Hash.String() != tc.expectedRef {
					t.Fatalf("Expected commit hash %s, got %s", tc.expectedRef, commit.Hash.String())
				}

				return
			}

			refName := plumbing.ReferenceName(tc.expectedRef)
			if tc.expectedRef != "" {
				ref, err := repo.Reference(refName, true)
				if err != nil {
					t.Fatalf("Failed to get reference %s: %v", refName, err)
				}

				if ref.Name().String() != tc.expectedRef {
					t.Fatalf("Expected reference %s, got %s", tc.expectedRef, ref.Name().String())
				}
			} else {
				_, err = repo.Reference(refName, true)
				if err == nil {
					t.Fatalf("Expected error for invalid reference %s, got nil", tc.expectedRef)
				}
			}
		})
	}
}

func TestCloneRepository_FullClone(t *testing.T) {
	t.Parallel()

	c, err := app.GetConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	auth, err := git.GetAuthMethod(cloneUrlTest, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	dir := t.TempDir()

	repo, err := git.CloneRepository(dir, cloneUrlTest, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules, 0)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil")
	}

	// Verify .git/shallow does NOT exist (full clone)
	shallowFile := dir + "/.git/shallow"
	if _, err := os.Stat(shallowFile); err == nil {
		t.Fatal("Expected full clone (no .git/shallow file), but .git/shallow exists")
	}

	// Verify we can iterate multiple commits (more than 1)
	iter, err := repo.CommitObjects()
	if err != nil {
		t.Fatalf("Failed to get commit objects: %v", err)
	}
	defer iter.Close()

	commitCount := 0
	_ = iter.ForEach(func(_ *object.Commit) error {
		commitCount++
		return nil
	})

	if commitCount <= 1 {
		t.Fatalf("Expected more than 1 commit in full clone, got %d", commitCount)
	}

	t.Logf("Full clone has %d commits", commitCount)

	// Verify checkout works
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	files, err := worktree.Filesystem.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed to read directory: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("No files in repository after full clone")
	}
}

func TestCloneRepository_ShallowClone(t *testing.T) {
	t.Parallel()

	c, err := app.GetConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	auth, err := git.GetAuthMethod(cloneUrlTest, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	dir := t.TempDir()

	repo, err := git.CloneRepository(dir, cloneUrlTest, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules, 1)
	if err != nil {
		t.Fatalf("Failed to shallow clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil")
	}

	// Verify .git/shallow EXISTS (shallow clone)
	shallowFile := dir + "/.git/shallow"
	if _, err := os.Stat(shallowFile); err != nil {
		t.Fatalf("Expected shallow clone (.git/shallow file), but it does not exist: %v", err)
	}

	// Verify commit count is limited
	iter, err := repo.CommitObjects()
	if err != nil {
		t.Fatalf("Failed to get commit objects: %v", err)
	}
	defer iter.Close()

	commitCount := 0
	_ = iter.ForEach(func(_ *object.Commit) error {
		commitCount++
		return nil
	})

	// With depth=1 and all branches/tags fetched, go-git fetches the tip commit of
	// each ref. The count will be much less than a full clone but more than 1 when
	// the repo has multiple branches. We use a generous upper bound here.
	if commitCount > 50 {
		t.Fatalf("Expected shallow clone to have significantly fewer commits than a full clone, got %d", commitCount)
	}

	t.Logf("Shallow clone (depth=1) has %d commit(s)", commitCount)

	// Verify checkout works and files are present
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	files, err := worktree.Filesystem.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed to read directory: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("No files in repository after shallow clone")
	}

	// Verify update with same shallow depth works
	repo, err = git.UpdateRepository(dir, cloneUrlTest, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules, 1)
	if err != nil {
		t.Fatalf("Failed to update shallow repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil after update")
	}

	// Still shallow after update
	if _, err := os.Stat(shallowFile); err != nil {
		t.Fatalf("Expected repository to remain shallow after update, but .git/shallow is gone: %v", err)
	}
}

func TestUpdateRepository_ShallowToFullTransition(t *testing.T) {
	t.Parallel()

	c, err := app.GetConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	auth, err := git.GetAuthMethod(cloneUrlTest, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	if auth != nil {
		t.Logf("Using auth method: %s", auth.Name())
	} else {
		t.Log("No auth method configured, using anonymous access")
	}

	dir := t.TempDir()

	// Step 1: Shallow clone (depth=1)
	repo, err := git.CloneRepository(dir, cloneUrlTest, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules, 1)
	if err != nil {
		t.Fatalf("Failed to shallow clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil after shallow clone")
	}

	// Verify it IS shallow
	shallowFile := dir + "/.git/shallow"
	if _, err := os.Stat(shallowFile); err != nil {
		t.Fatalf("Expected shallow clone (.git/shallow file), but it does not exist: %v", err)
	}

	// Count commits in shallow clone
	iter, err := repo.CommitObjects()
	if err != nil {
		t.Fatalf("Failed to get commit objects: %v", err)
	}

	shallowCommitCount := 0
	_ = iter.ForEach(func(_ *object.Commit) error {
		shallowCommitCount++
		return nil
	})
	iter.Close()

	t.Logf("Shallow clone (depth=1) has %d commit(s)", shallowCommitCount)

	// Step 2: Update with depth=0, which should trigger re-clone (shallow → full transition)
	repo, err = git.UpdateRepository(dir, cloneUrlTest, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules, 0)
	if err != nil {
		t.Fatalf("Failed to update repository with full depth: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil after shallow→full transition")
	}

	// Verify it is NOT shallow anymore
	if _, err := os.Stat(shallowFile); err == nil {
		t.Fatal("Expected full clone after transition (no .git/shallow), but .git/shallow still exists")
	}

	// Verify commit count increased
	iter, err = repo.CommitObjects()
	if err != nil {
		t.Fatalf("Failed to get commit objects after transition: %v", err)
	}

	fullCommitCount := 0
	_ = iter.ForEach(func(_ *object.Commit) error {
		fullCommitCount++
		return nil
	})
	iter.Close()

	t.Logf("After shallow→full transition: %d commit(s)", fullCommitCount)

	if fullCommitCount <= shallowCommitCount {
		t.Fatalf("Expected more commits after full transition, got %d (was %d)", fullCommitCount, shallowCommitCount)
	}

	// Verify worktree still works
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	files, err := worktree.Filesystem.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed to read directory: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("No files in repository after shallow→full transition")
	}
}

func TestUpdateRepository_FullToShallowTransition(t *testing.T) {
	t.Parallel()

	c, err := app.GetConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	auth, err := git.GetAuthMethod(cloneUrlTest, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	dir := t.TempDir()

	// Step 1: Full clone (depth=0)
	repo, err := git.CloneRepository(dir, cloneUrlTest, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules, 0)
	if err != nil {
		t.Fatalf("Failed to clone repository (full): %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil after full clone")
	}

	// Verify it's NOT shallow
	shallowFile := dir + "/.git/shallow"
	if _, err := os.Stat(shallowFile); err == nil {
		t.Fatal("Expected full clone (no .git/shallow), but found it")
	}

	// Count commits in full clone
	iter, err := repo.CommitObjects()
	if err != nil {
		t.Fatalf("Failed to get commit objects: %v", err)
	}

	fullCommitCount := 0
	_ = iter.ForEach(func(_ *object.Commit) error {
		fullCommitCount++
		return nil
	})
	iter.Close()

	t.Logf("Full clone has %d commits", fullCommitCount)

	// Step 2: Update with depth=1, which should trigger re-clone (full → shallow transition)
	repo, err = git.UpdateRepository(dir, cloneUrlTest, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules, 1)
	if err != nil {
		t.Fatalf("Failed to update repository with shallow depth: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil after full→shallow transition")
	}

	// Verify it IS now shallow
	if _, err := os.Stat(shallowFile); err != nil {
		t.Fatalf("Expected shallow clone after transition, but .git/shallow does not exist: %v", err)
	}

	// Verify commit count decreased
	iter, err = repo.CommitObjects()
	if err != nil {
		t.Fatalf("Failed to get commit objects after transition: %v", err)
	}

	shallowCommitCount := 0
	_ = iter.ForEach(func(_ *object.Commit) error {
		shallowCommitCount++
		return nil
	})
	iter.Close()

	t.Logf("After full→shallow transition: %d commit(s)", shallowCommitCount)

	if shallowCommitCount >= fullCommitCount && fullCommitCount > 1 {
		t.Fatalf("Expected fewer commits after shallow transition, got %d (was %d)", shallowCommitCount, fullCommitCount)
	}

	// Verify worktree still works
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	files, err := worktree.Filesystem.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed to read directory: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("No files in repository after full→shallow transition")
	}
}
