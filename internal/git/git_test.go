package git_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/kimdre/doco-cd/internal/config/app"

	"github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/encryption"
)

const (
	cloneUrl         = "https://github.com/kimdre/doco-cd.git"
	cloneUrlTest     = "https://github.com/kimdre/doco-cd_tests.git"
	cloneUrlSSH      = "git@github.com:kimdre/doco-cd.git"
	remoteMainBranch = "refs/remotes/origin/main"
	remoteTagRef     = "refs/tags/v0.81.0-rc.1"
	tagRef           = "v0.81.0-rc.1"
	invalidRef       = "refs/heads/invalid"
	invalidTagRef    = "refs/tags/invalid"
	commitSHARef     = "bb8864f3fb30cdd36a109f52bc4ab961ec40f5d6"
)

func TestHttpTokenAuth(t *testing.T) {
	testCases := []struct {
		name         string
		username     string
		token        string
		expectNil    bool
		expectedUser string
		expectedErr  error
	}{
		{
			name:         "Valid token defaults username",
			token:        "ghp_test123456",
			expectNil:    false,
			expectedUser: git.DefaultHTTPAuthUser,
			expectedErr:  nil,
		},
		{
			name:         "Custom username (e.g. GitLab deploy token)",
			username:     "gitlab+deploy-token-123",
			token:        "gldt_test123456",
			expectNil:    false,
			expectedUser: "gitlab+deploy-token-123",
			expectedErr:  nil,
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

			auth := git.HttpTokenAuth(tc.username, tc.token)

			if tc.expectNil && auth != nil {
				t.Fatal("Expected nil auth for empty token")
			}

			if !tc.expectNil && auth == nil {
				t.Fatal("Expected non-nil auth for valid token")
			}

			if auth == nil {
				return
			}

			if auth.Name() != "http-basic-auth" {
				t.Fatalf("Expected auth name 'http-basic-auth', got '%s'", auth.Name())
			}

			basicAuth, ok := auth.(*githttp.BasicAuth)
			if !ok {
				t.Fatalf("Expected *githttp.BasicAuth, got %T", auth)
			}

			if basicAuth.Username != tc.expectedUser {
				t.Fatalf("Expected username '%s', got '%s'", tc.expectedUser, basicAuth.Username)
			}
		})
	}
}

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

func TestCloneRepository_WithSubmodule(t *testing.T) {
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

	repo, err := git.CloneRepository(t.TempDir(), cloneUrlTest, "with-submodule", false, c.HttpProxy, auth, true, 0)
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

func TestUpdateRepository_WithSubmodule(t *testing.T) {
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

	repo, err := git.CloneRepository(t.TempDir(), cloneUrlTest, git.MainBranch, false, c.HttpProxy, auth, true, 0)
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

	repo, err = git.UpdateRepository(worktree.Filesystem.Root(), cloneUrlTest, "with-submodule", false, c.HttpProxy, auth, true, 0)
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

	c, err := app.GetConfig()
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

	repo, err := git.CloneRepository(t.TempDir(), cloneUrl, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules, 0)
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

	c, err := app.GetConfig()
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

	repo, err := git.CloneRepository(t.TempDir(), url, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules, 0)
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

	repo, err = git.UpdateRepository(worktree.Filesystem.Root(), url, "alternative", false, c.HttpProxy, auth, c.GitCloneSubmodules, 0)
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

	c, err := app.GetConfig()
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

	repo, err := git.CloneRepository(t.TempDir(), url, git.MainBranch, false, c.HttpProxy, auth, c.GitCloneSubmodules, 0)
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
		encryptedKeyPassphrase = app.Name
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
		{
			name:     "SSH URL with non-default port stays unchanged",
			sshUrl:   "ssh://git@github.com:2222/user/repo.git",
			expected: "ssh://git@github.com:2222/user/repo.git",
		},
		{
			name:     "SSH URL with ",
			sshUrl:   "ssh://git@gitea:2222/user/repo.git",
			expected: "ssh://git@gitea:2222/user/repo.git",
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
		{
			cloneURL: "http://git.example.com/infra/alpha/local/netbird-doco.git",
			expected: "git.example.com/infra/alpha/local/netbird-doco",
		},
		{
			cloneURL: "git@gitlab.com:gitlab-org/5-minute-production-app/sandbox/cats.git",
			expected: "gitlab.com/gitlab-org/5-minute-production-app/sandbox/cats",
		},
		{
			cloneURL: "https://gitlab.com/gitlab-org/5-minute-production-app/sandbox/cats.git",
			expected: "gitlab.com/gitlab-org/5-minute-production-app/sandbox/cats",
		},
		// Local filesystem repositories (file:// URLs)
		{
			cloneURL: "file:///data/local-repos/my-app",
			expected: "data/local-repos/my-app",
		},
		{
			cloneURL: "file:///data/local-repos/my-app.git",
			expected: "data/local-repos/my-app",
		},
		{
			cloneURL: "file:///my-app",
			expected: "my-app",
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

	// Step 2: Update with depth=0 — should trigger re-clone (shallow → full transition)
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

	// Step 2: Update with depth=1 — should trigger re-clone (full → shallow transition)
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

func TestFormatGitErrorMessage_DecodesLocalizedAzureDevOpsError(t *testing.T) {
	t.Parallel()

	errMsg := `authentication required (or check https://dev.azure.com for error details)
remote: {"message":"\u041d\u0435\u0434\u043e\u043f\u0443\u0441\u0442\u0438\u043c\u0430\u044f \u0432\u0435\u0442\u043a\u0430","typeName":"Microsoft.TeamFoundation.SourceControl.WebServer.InvalidRefNameException","errorCode":12345}`
	err := errors.New(errMsg)

	formatted := git.FormatGitErrorMessage(err)

	if !strings.Contains(formatted, "Недопустимая ветка") {
		t.Errorf("formatted error should contain decoded message, got: %s", formatted)
	}

	if !strings.Contains(formatted, "typeName=Microsoft.TeamFoundation.SourceControl.WebServer.InvalidRefNameException") {
		t.Errorf("formatted error should contain typeName, got: %s", formatted)
	}

	if !strings.Contains(formatted, "errorCode=12345") {
		t.Errorf("formatted error should contain errorCode, got: %s", formatted)
	}

	if strings.Contains(formatted, "\\u041d") {
		t.Errorf("formatted error should not contain escaped unicode, got: %s", formatted)
	}
}

func TestFormatGitErrorMessage_DecodesJSONStringError(t *testing.T) {
	t.Parallel()

	errMsg := `authentication required: "\u041d\u0435\u0434\u043e\u0441\u0442\u0443\u043f\u043d\u043e"`
	err := errors.New(errMsg)

	formatted := git.FormatGitErrorMessage(err)

	if !strings.Contains(formatted, "Недоступно") {
		t.Errorf("formatted error should contain decoded JSON string message, got: %s", formatted)
	}

	if strings.Contains(formatted, "\\u041d") {
		t.Errorf("formatted error should not contain escaped unicode, got: %s", formatted)
	}
}

func TestFormatGitErrorMessage_ReturnsOriginalWhenNoJSON(t *testing.T) {
	t.Parallel()

	errMsg := "simple error message without JSON"
	err := errors.New(errMsg)

	formatted := git.FormatGitErrorMessage(err)

	if formatted != errMsg {
		t.Errorf("formatted error should match original when no JSON found, got: %s", formatted)
	}
}

func TestFormatGitErrorMessage_NilError(t *testing.T) {
	t.Parallel()

	formatted := git.FormatGitErrorMessage(nil)

	if formatted != "" {
		t.Errorf("formatted nil error should be empty string, got: %s", formatted)
	}
}

// initLocalTestRepo creates a bare-metal (non-bare) local git repository at path
// with an initial commit on the "main" branch, returning the go-git repository handle.
func initLocalTestRepo(t *testing.T, path string) *gogit.Repository {
	t.Helper()

	repo, err := gogit.PlainInit(path, false)
	if err != nil {
		t.Fatalf("failed to init local test repo: %v", err)
	}

	// Point HEAD at "main" before the first commit so it lands there directly,
	// avoiding a checkout of a not-yet-existing branch on an empty repository.
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("failed to set HEAD to main: %v", err)
	}

	commitLocalTestFile(t, repo, path, "README.md", "initial\n", "initial commit")

	return repo
}

// commitLocalTestFile writes relPath under repoPath, stages and commits it, returning the new commit hash.
func commitLocalTestFile(t *testing.T, repo *gogit.Repository, repoPath, relPath, content, msg string) plumbing.Hash {
	t.Helper()

	filePath := filepath.Join(repoPath, relPath)
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write %s: %v", relPath, err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := wt.Add(relPath); err != nil {
		t.Fatalf("failed to add %s: %v", relPath, err)
	}

	hash, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "local-fs-test",
			Email: "local-fs-test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to commit %q: %v", msg, err)
	}

	return hash
}

func TestCloneRepository_LocalFileURL(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	dstPath := t.TempDir()

	auth, err := git.GetAuthMethod("file://"+srcPath, "", "", "")
	if err != nil {
		t.Fatalf("GetAuthMethod() error = %v", err)
	}

	if auth != nil {
		t.Fatalf("GetAuthMethod() = %v, want nil for local file URL", auth)
	}

	repo, err := git.CloneRepository(dstPath, "file://"+srcPath, git.MainBranch, false, transport.ProxyOptions{}, auth, false, 0)
	if err != nil {
		t.Fatalf("CloneRepository() error = %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}

	if head.Name().Short() != "main" {
		t.Fatalf("Head() branch = %q, want main", head.Name().Short())
	}
}

func TestFetchRepository_LocalFileURL_DetectsNewCommit(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	srcRepo := initLocalTestRepo(t, srcPath)

	dstPath := t.TempDir()

	repo, err := git.CloneRepository(dstPath, "file://"+srcPath, git.MainBranch, false, transport.ProxyOptions{}, nil, false, 0)
	if err != nil {
		t.Fatalf("CloneRepository() error = %v", err)
	}

	matches, err := git.MatchesHead(dstPath, git.MainBranch)
	if err != nil {
		t.Fatalf("MatchesHead() error = %v", err)
	}

	if !matches {
		t.Fatal("MatchesHead() = false right after clone, want true")
	}

	// Add a new commit to the source repository and verify the clone detects it via fetch.
	newHash := commitLocalTestFile(t, srcRepo, srcPath, "CHANGED.md", "changed\n", "second commit")

	if err := git.FetchRepository(repo, "file://"+srcPath, false, transport.ProxyOptions{}, nil, 0); err != nil {
		t.Fatalf("FetchRepository() error = %v", err)
	}

	matches, err = git.MatchesHead(dstPath, git.MainBranch)
	if err != nil {
		t.Fatalf("MatchesHead() error = %v", err)
	}

	if matches {
		t.Fatal("MatchesHead() = true after fetch but before checkout, want false since the source repo has a new commit")
	}

	if _, err := git.UpdateRepository(dstPath, "file://"+srcPath, git.MainBranch, false, transport.ProxyOptions{}, nil, false, 0); err != nil {
		t.Fatalf("UpdateRepository() error = %v", err)
	}

	matches, err = git.MatchesHead(dstPath, git.MainBranch)
	if err != nil {
		t.Fatalf("MatchesHead() error = %v", err)
	}

	if !matches {
		t.Fatal("MatchesHead() = false after fetch+update, want true")
	}

	updatedRepo, err := git.OpenRepository(dstPath)
	if err != nil {
		t.Fatalf("OpenRepository() error = %v", err)
	}

	head, err := updatedRepo.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}

	if head.Hash() != newHash {
		t.Fatalf("Head() = %s, want %s", head.Hash(), newHash)
	}
}

func TestCloneRepository_LocalFileURL_BareRepository(t *testing.T) {
	t.Parallel()

	// Populate a bare repository by cloning a normal one into it.
	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	barePath := filepath.Join(t.TempDir(), "bare.git")
	if _, err := gogit.PlainClone(barePath, true, &gogit.CloneOptions{URL: "file://" + srcPath}); err != nil {
		t.Fatalf("failed to create bare test repo: %v", err)
	}

	dstPath := t.TempDir()

	repo, err := git.CloneRepository(dstPath, "file://"+barePath, git.MainBranch, false, transport.ProxyOptions{}, nil, false, 0)
	if err != nil {
		t.Fatalf("CloneRepository() from bare repo error = %v", err)
	}

	if _, err = repo.Head(); err != nil {
		t.Fatalf("Head() error = %v", err)
	}
}

func TestCloneRepository_LocalFileURL_GitDirFile(t *testing.T) {
	t.Parallel()

	// Simulate a submodule/worktree layout: the working tree holds a ".git" *file*
	// pointing at the real git directory stored elsewhere.
	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	movedGitDir := filepath.Join(t.TempDir(), "modules", "app")
	if err := os.MkdirAll(filepath.Dir(movedGitDir), 0o750); err != nil {
		t.Fatalf("failed to create git dir parent: %v", err)
	}

	if err := os.Rename(filepath.Join(srcPath, ".git"), movedGitDir); err != nil {
		t.Fatalf("failed to move git dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(srcPath, ".git"), []byte("gitdir: "+movedGitDir+"\n"), 0o600); err != nil {
		t.Fatalf("failed to write .git file: %v", err)
	}

	dstPath := t.TempDir()

	repo, err := git.CloneRepository(dstPath, "file://"+srcPath, git.MainBranch, false, transport.ProxyOptions{}, nil, false, 0)
	if err != nil {
		t.Fatalf("CloneRepository() with .git file error = %v", err)
	}

	if _, err = repo.Head(); err != nil {
		t.Fatalf("Head() error = %v", err)
	}
}

func TestCloneRepository_LocalFileURL_IgnoresShallowDepth(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	srcRepo := initLocalTestRepo(t, srcPath)
	commitLocalTestFile(t, srcRepo, srcPath, "SECOND.md", "second\n", "second commit")
	commitLocalTestFile(t, srcRepo, srcPath, "THIRD.md", "third\n", "third commit")

	dstPath := t.TempDir()

	// The in-process transport does not implement git's shallow capability, so a
	// requested depth must be ignored rather than failing the clone.
	repo, err := git.CloneRepository(dstPath, "file://"+srcPath, git.MainBranch, false, transport.ProxyOptions{}, nil, false, 1)
	if err != nil {
		t.Fatalf("CloneRepository() with depth error = %v", err)
	}

	if _, err = os.Stat(filepath.Join(dstPath, ".git", "shallow")); !os.IsNotExist(err) {
		t.Fatalf("clone of local repository must not be shallow, stat shallow file err = %v", err)
	}

	// A subsequent depth-limited update must not re-clone in a loop or fail.
	if _, err = git.UpdateRepository(dstPath, "file://"+srcPath, git.MainBranch, false, transport.ProxyOptions{}, nil, false, 1); err != nil {
		t.Fatalf("UpdateRepository() with depth error = %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}

	if head.Hash().IsZero() {
		t.Fatal("Head() returned zero hash")
	}
}
