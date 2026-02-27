package git_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/encryption"
)

const (
	cloneUrl         = "https://github.com/kimdre/doco-cd.git"
	cloneUrlTest     = "https://github.com/kimdre/doco-cd_tests.git"
	cloneUrlSSH      = "git@github.com:kimdre/doco-cd.git"
	remoteMainBranch = "refs/remotes/origin/main"
	remoteTagRef     = "refs/tags/v0.15.0"
	tagRef           = "v0.15.0"
	invalidRef       = "refs/heads/invalid"
	invalidTagRef    = "refs/tags/invalid"
	commitSHARef     = "bb8864f3fb30cdd36a109f52bc4ab961ec40f5d6"
)

func TestHttpTokenAuth(t *testing.T) {
	testCases := []struct {
		name        string
		token       string
		expectNil   bool
		expectedErr error
	}{
		{
			name:        "Valid token",
			token:       "ghp_test123456",
			expectNil:   false,
			expectedErr: nil,
		},
		{
			name:        "Empty token",
			token:       "",
			expectNil:   true,
			expectedErr: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			auth := git.HttpTokenAuth(tc.token)

			if tc.expectNil && auth != nil {
				t.Fatal("Expected nil auth for empty token")
			}

			if !tc.expectNil && auth == nil {
				t.Fatal("Expected non-nil auth for valid token")
			}

			if auth != nil && auth.Name() != "http-basic-auth" {
				t.Fatalf("Expected auth name 'http-basic-auth', got '%s'", auth.Name())
			}
		})
	}
}

func TestCloneRepository(t *testing.T) {
	t.Parallel()

	c, err := config.GetAppConfig()
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

			repo, err := git.CloneRepository(t.TempDir(), tc.cloneUrl, tc.reference, false, c.HttpProxy, auth, c.GitCloneSubmodules)
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

func TestCloneRepository_WithSubmodule(t *testing.T) {
	t.Parallel()

	c, err := config.GetAppConfig()
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

	repo, err := git.CloneRepository(t.TempDir(), cloneUrlTest, "with-submodule", false, c.HttpProxy, auth, true)
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

	submodules, err := worktree.Submodules()
	if err != nil {
		t.Fatalf("Failed to get submodules: %v", err)
	}

	if len(submodules) == 0 {
		t.Fatal("No submodules found, but expected one")
	}

	submodule := submodules[0]

	if submodule.Config().Path != "doco-cd_tests" {
		t.Fatalf("Expected submodule path 'doco-cd_tests', got '%s'", submodule.Config().Path)
	}

	// Check if submodule is initialized by reading the README.md file in the submodule directory
	subRepo, err := submodule.Repository()
	if err != nil {
		t.Fatalf("Failed to get submodule repository: %v", err)
	}

	subWorktree, err := subRepo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get submodule worktree: %v", err)
	}

	files, err := subWorktree.Filesystem.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed to read submodule directory: %v", err)
	}

	foundReadme := false

	for _, file := range files {
		if file.Name() == "README.md" {
			foundReadme = true
			break
		}
	}

	if !foundReadme {
		t.Fatal("Submodule is not initialized, README.md not found")
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

			c, err := config.GetAppConfig()
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

			repo, err := git.CloneRepository(t.TempDir(), tc.cloneUrl, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules)
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

			repo, err = git.UpdateRepository(worktree.Filesystem.Root(), tc.cloneUrl, tc.branchRef, false, c.HttpProxy, auth, c.GitCloneSubmodules)
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

func TestUpdateRepository_WithSubmodule(t *testing.T) {
	t.Parallel()

	c, err := config.GetAppConfig()
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

	repo, err := git.CloneRepository(t.TempDir(), cloneUrlTest, git.MainBranch, false, c.HttpProxy, auth, true)
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

	// Check if any submodules exist before update
	submodules, err := worktree.Submodules()
	if err != nil {
		t.Fatalf("Failed to get submodules: %v", err)
	}

	if len(submodules) != 0 {
		t.Fatal("Expected no submodules before update, but found some")
	}

	repo, err = git.UpdateRepository(worktree.Filesystem.Root(), cloneUrlTest, "with-submodule", false, c.HttpProxy, auth, true)
	if err != nil {
		t.Fatalf("Failed to update repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil")
	}

	worktree, err = repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	submodules, err = worktree.Submodules()
	if err != nil {
		t.Fatalf("Failed to get submodules: %v", err)
	}

	if len(submodules) == 0 {
		t.Fatal("No submodules found, but expected one")
	}

	submodule := submodules[0]

	if submodule.Config().Path != "doco-cd_tests" {
		t.Fatalf("Expected submodule path 'doco-cd_tests', got '%s'", submodule.Config().Path)
	}

	// Check if submodule is initialized by reading the README.md file in the submodule directory
	subRepo, err := submodule.Repository()
	if err != nil {
		t.Fatalf("Failed to get submodule repository: %v", err)
	}

	subWorktree, err := subRepo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get submodule worktree: %v", err)
	}

	files, err := subWorktree.Filesystem.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed to read submodule directory: %v", err)
	}

	foundReadme := false

	for _, file := range files {
		if file.Name() == "README.md" {
			foundReadme = true
			break
		}
	}

	if !foundReadme {
		t.Fatal("Submodule is not initialized, README.md not found")
	}
}

func TestGetReferenceSet(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name              string
		localRef          string
		expectedLocalRef  string
		expectedRemoteRef string
	}{
		{
			name:              "Branch",
			localRef:          "main",
			expectedLocalRef:  git.MainBranch,
			expectedRemoteRef: remoteMainBranch,
		},
		{
			name:              "Branch Reference",
			localRef:          git.MainBranch,
			expectedLocalRef:  git.MainBranch,
			expectedRemoteRef: remoteMainBranch,
		},
		{
			name:              "Tag",
			localRef:          tagRef,
			expectedLocalRef:  remoteTagRef,
			expectedRemoteRef: remoteTagRef,
		},
		{
			name:              "Commit SHA",
			localRef:          commitSHARef,
			expectedLocalRef:  commitSHARef,
			expectedRemoteRef: "", // For commit SHA, there is no remote reference
		},
	}

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	auth, err := git.GetAuthMethod(cloneUrl, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	if auth != nil {
		t.Logf("Using auth method: %s", auth.Name())
	} else {
		t.Log("No auth method configured, using anonymous access")
	}

	repo, err := git.CloneRepository(t.TempDir(), cloneUrl, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil")
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			refSet, err := git.GetReferenceSet(repo, tc.localRef)
			if err != nil {
				t.Fatalf("Failed to get reference set: %v", err)
			}

			if refSet.LocalRef.String() == "" || (tc.expectedRemoteRef != "" && refSet.RemoteRef.String() == "") {
				t.Fatalf("Reference set is incomplete: localRef: %s, remoteRef: %s", refSet.LocalRef.String(), refSet.RemoteRef.String())
			}

			if refSet.LocalRef.String() != tc.expectedLocalRef {
				t.Fatalf("Expected local reference %s, got %s", tc.expectedLocalRef, refSet.LocalRef.String())
			}

			if refSet.RemoteRef.String() != tc.expectedRemoteRef {
				t.Fatalf("Expected remote reference %s, got %s", tc.expectedRemoteRef, refSet.RemoteRef.String())
			}
		})
	}
}

func TestUpdateRepository_KeepUntrackedFiles(t *testing.T) {
	t.Parallel()

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	url := cloneUrlTest

	auth, err := git.GetAuthMethod(url, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	if auth != nil {
		t.Logf("Using auth method: %s", auth.Name())
	} else {
		t.Log("No auth method configured, using anonymous access")
	}

	repo, err := git.CloneRepository(t.TempDir(), url, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules)
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

	// Add a new file to the cloned repository
	newFileName := "new.txt"

	_, err = worktree.Filesystem.Create(newFileName)
	if err != nil {
		t.Fatalf("Failed to create new file: %v", err)
	}

	repo, err = git.UpdateRepository(worktree.Filesystem.Root(), url, "alternative", false, c.HttpProxy, auth, c.GitCloneSubmodules)
	if err != nil {
		t.Fatalf("Failed to update repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil after update")
	}

	files, err := worktree.Filesystem.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed to read directory: %v", err)
	}

	foundNewFile := false

	for _, file := range files {
		if file.Name() == newFileName {
			foundNewFile = true
			break
		}
	}

	if !foundNewFile {
		t.Fatal("Untracked file was removed during update")
	}
}

func TestGetLatestCommit(t *testing.T) {
	t.Parallel()

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	url := cloneUrl

	auth, err := git.GetAuthMethod(url, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	if auth != nil {
		t.Logf("Using auth method: %s", auth.Name())
	} else {
		t.Log("No auth method configured, using anonymous access")
	}

	repo, err := git.CloneRepository(t.TempDir(), url, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil")
	}

	commit, err := git.GetLatestCommit(repo, git.MainBranch)
	if err != nil {
		t.Fatalf("Failed to get latest commit: %v", err)
	}

	if commit == "" {
		t.Fatal("Commit hash is empty")
	}

	t.Log(commit)
}

func TestGetChangedFilesBetweenCommits(t *testing.T) {
	t.Parallel()

	var (
		commitOld                = plumbing.NewHash("f8c5992297bf70eb01f0ba40d062896b1f48dc65")
		commitNew                = plumbing.NewHash("e72ef851774e50b82c173fd36cfcf9a88355c592")
		expectedChangedDirectory = "html"
		expectedChangedFile      = filepath.Join(expectedChangedDirectory, "index.html")
	)

	tmpDir := t.TempDir()

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	url := cloneUrlTest

	auth, err := git.GetAuthMethod(url, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("Failed to get auth method: %v", err)
	}

	if auth != nil {
		t.Logf("Using auth method: %s", auth.Name())
	} else {
		t.Log("No auth method configured, using anonymous access")
	}

	repo, err := git.CloneRepository(tmpDir, url, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	changedFiles, err := git.GetChangedFilesBetweenCommits(repo, commitOld, commitNew)
	if err != nil {
		t.Fatalf("Failed to get changed files: %v", err)
	}

	if len(changedFiles) == 0 {
		t.Fatal("No changed files found, but expected one changed file")
	}

	for _, file := range changedFiles {
		if file.From.Path() != expectedChangedFile {
			t.Errorf("Expected file %s, got %s", expectedChangedFile, file.From.Path())
		}

		if file.To.Path() != expectedChangedFile {
			t.Errorf("Expected file %s, got %s", expectedChangedFile, file.To.Path())
		}
	}

	var changedFilePaths []string
	for _, file := range changedFiles {
		changedFilePaths = append(changedFilePaths, file.To.Path())
	}

	t.Logf("Changed files: %v", changedFilePaths)
	t.Logf("testDir: %s", expectedChangedDirectory)

	hasChanged, err := git.HasChangesInSubdir(changedFiles, tmpDir, expectedChangedDirectory)
	if err != nil {
		t.Fatalf("Failed to check changes in subdir: %v", err)
	}

	if !hasChanged {
		t.Errorf("Expected changes in subdir %s, but found none", expectedChangedDirectory)
	}
}

func TestSSHAuth(t *testing.T) {
	t.Parallel()

	const (
		encryptedKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAACmFlczI1Ni1jdHIAAAAGYmNyeXB0AAAAGAAAABA+Zz/91P
rp2u7NvTWBtLI0AAAAGAAAAAEAAAAzAAAAC3NzaC1lZDI1NTE5AAAAIFyEIiKcYAJl82Ga
40hVJoKO1qOvVfekORkGLSsKFnF7AAAAoBgOn6fvoLqNvcj0QMyuZTYVJEm9YXs8zNkG+9
suGsdNHOvMRQWLzq9VJiJUyOG29zayIQ4Q3pZlcoRINpUI9yl4/eFza7P4MEHDVBLF531K
X3nAnZomTg2czfus92AmR+3kYDWvBE1WkpieAaRfVTuBtNcB41rOAZMLQ001zhVF2qdb+D
+tvLTkrbIyLPEbZOBHuCH+mVgPefYCRXsB9Nw=
-----END OPENSSH PRIVATE KEY-----`
		encryptedKeyPassphrase = config.AppName
		unencryptedKey         = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACCU6Sk58h0kd2bUvHHvyS1JQiLgBf6yKaIbpGlK8TEfVAAAAJgBQMSpAUDE
qQAAAAtzc2gtZWQyNTUxOQAAACCU6Sk58h0kd2bUvHHvyS1JQiLgBf6yKaIbpGlK8TEfVA
AAAEBBVspZHjWj6Np5szQQHB6w+1X3ZOatDcMmcnm1+R9J9pTpKTnyHSR3ZtS8ce/JLUlC
IuAF/rIpohukaUrxMR9UAAAADmtpbUBraW0tZmVkb3JhAQIDBAUGBw==
-----END OPENSSH PRIVATE KEY-----`
	)

	testCases := []struct {
		name        string
		privateKey  string
		passphrase  string
		expectedErr string
	}{
		{
			name:        "Encrypted ED25519 key",
			privateKey:  encryptedKey,
			passphrase:  encryptedKeyPassphrase,
			expectedErr: "",
		},
		{
			name:        "Missing passphrase for encrypted key",
			privateKey:  encryptedKey,
			passphrase:  "",
			expectedErr: "failed to create SSH public keys: bcrypt_pbkdf: empty password",
		},
		{
			name:        "Unencrypted ED25519 key",
			privateKey:  unencryptedKey,
			passphrase:  "",
			expectedErr: "",
		},
		{
			name:        "Unencrypted ED25519 key with passphrase",
			privateKey:  unencryptedKey,
			passphrase:  "test",
			expectedErr: "",
		},
		{
			name:        "Missing private key",
			privateKey:  "",
			passphrase:  "",
			expectedErr: "ssh URL requires SSH_PRIVATE_KEY to be set",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			auth, err := git.SSHAuth(tc.privateKey, tc.passphrase)
			if err != nil {
				if tc.expectedErr == "" {
					t.Fatalf("Expected no error, got %v", err)
				}

				if err.Error() == tc.expectedErr {
					return
				}

				t.Fatalf("Expected error %v, got %v", tc.expectedErr, err.Error())
			} else if tc.expectedErr != "" {
				t.Fatalf("Expected error %v, got none", tc.expectedErr)
			}

			if auth == nil {
				if tc.expectedErr != "auth empty" {
					t.Fatal("Expected auth to be non-nil")
				}
			}

			if auth.Name() != "ssh-public-keys" {
				t.Fatalf("Expected auth name 'ssh-public-keys', got '%s'", auth.Name())
			}
		})
	}
}

func TestConvertSSHUrl(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		sshUrl   string
		expected string
	}{
		{
			name:     "Valid SSH URL",
			sshUrl:   "git@github.com:user/repo.git",
			expected: "ssh://git@github.com/user/repo.git",
		},
		{
			name:     "Valid SSH URL without .git",
			sshUrl:   "git@github.com:user/repo",
			expected: "ssh://git@github.com/user/repo",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := git.ConvertSSHUrl(tc.sshUrl)
			if tc.expected == "" {
				if result != tc.expected {
					t.Fatalf("Expected empty string for invalid URL, got %s", result)
				}
			}

			if result != tc.expected {
				t.Fatalf("Expected %s, got %s", tc.expected, result)
			}
		})
	}
}

func TestGetRepoName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cloneURL string
		expected string
	}{
		{
			cloneURL: "https://github.com/kimdre/doco-cd_tests.git",
			expected: "github.com/kimdre/doco-cd_tests",
		},
		{
			cloneURL: "https://user:password@github.com/kimdre/doco-cd_tests.git", // #nosec G101 -- This is a test URL, not a real token
			expected: "github.com/kimdre/doco-cd_tests",
		},
		{
			cloneURL: "http://git.example.com/doco-cd.git",
			expected: "git.example.com/doco-cd",
		},
		// SSH SCP-like
		{
			cloneURL: "git@github.com:kimdre/doco-cd_tests.git",
			expected: "github.com/kimdre/doco-cd_tests",
		},
		// SSH URL
		{
			cloneURL: "ssh://git@github.com/kimdre/doco-cd_tests.git",
			expected: "github.com/kimdre/doco-cd_tests",
		},
		{
			cloneURL: "ssh://github.com/kimdre/doco-cd_tests.git",
			expected: "github.com/kimdre/doco-cd_tests",
		},
		// Token-injected HTTPS
		{
			cloneURL: "https://oauth2:TOKEN@github.com/kimdre/doco-cd_tests.git", // #nosec G101 -- This is a test URL, not a real token
			expected: "github.com/kimdre/doco-cd_tests",
		},
	}
	for _, tt := range tests {
		t.Run(tt.cloneURL, func(t *testing.T) {
			result := git.GetRepoName(tt.cloneURL)
			if result != tt.expected {
				t.Errorf("GetRepoName failed for %s: expected %s, got %s", tt.cloneURL, tt.expected, result)
			}
		})
	}
}
