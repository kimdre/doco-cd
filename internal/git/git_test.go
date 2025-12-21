package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/encryption"
)

const (
	cloneUrl            = "https://github.com/kimdre/doco-cd.git"
	cloneUrlTest        = "https://github.com/kimdre/doco-cd_tests.git"
	cloneUrlSSH         = "git@github.com:kimdre/doco-cd.git"
	remoteMainBranch    = "refs/remotes/origin/main"
	validBranchRef      = MainBranch
	validBranchRefShort = "main"
	validTagRef         = "refs/tags/v0.15.0"
	validTagRefShort    = "v0.15.0"
	invalidRef          = "refs/heads/invalid"
	invalidTagRef       = "refs/tags/invalid"
)

func TestGetAuthUrl(t *testing.T) {
	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed To get app config: %v", err)
	}

	expectedUrl := fmt.Sprintf("https://%s:%s@github.com/kimdre/doco-cd.git", c.AuthType, c.GitAccessToken)

	authUrl := GetAuthUrl(
		"https://github.com/kimdre/doco-cd.git",
		c.AuthType,
		c.GitAccessToken,
	)

	if authUrl != expectedUrl {
		t.Fatalf("Expected %s, got %s", expectedUrl, authUrl)
	}
}

func TestCloneRepository(t *testing.T) {
	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	testCases := []struct {
		name       string
		cloneUrl   string
		privateKey string
		passphrase string
		skip       bool
	}{
		{
			name:       "HTTP clone",
			cloneUrl:   cloneUrl,
			privateKey: "",
			passphrase: "",
			skip:       false,
		},
		{
			name:       "SSH clone",
			cloneUrl:   cloneUrlSSH,
			privateKey: c.SSHPrivateKey,
			passphrase: c.SSHPrivateKeyPassphrase,
			skip:       c.SSHPrivateKey == "",
		},
	}

	for _, tc := range testCases {
		// capture range variable
		t.Run(tc.name, func(t *testing.T) {
			if tc.skip {
				t.Skip("SSH private key not set, skipping SSH clone test")
			}

			repo, err := CloneRepository(t.TempDir(), tc.cloneUrl, validBranchRef, false, c.HttpProxy, tc.privateKey, tc.passphrase)
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
			branchRef:   validBranchRef,
			expectedRef: remoteMainBranch,
			expectedErr: nil,
		},
		{
			name:        "Valid short branch ref",
			cloneUrl:    cloneUrl,
			branchRef:   validBranchRefShort,
			expectedRef: remoteMainBranch,
			expectedErr: nil,
		},
		{
			name:        "Valid tag ref",
			cloneUrl:    cloneUrl,
			privateRepo: false,
			branchRef:   validTagRef,
			expectedRef: validTagRef,
			expectedErr: nil,
		},
		{
			name:        "Valid short tag ref",
			cloneUrl:    cloneUrl,
			privateRepo: false,
			branchRef:   validTagRefShort,
			expectedRef: validTagRef,
			expectedErr: nil,
		},
		{
			name:        "Invalid branch ref",
			cloneUrl:    cloneUrl,
			privateRepo: false,
			branchRef:   invalidRef,
			expectedRef: "",
			expectedErr: ErrInvalidReference,
		},
		{
			name:        "Invalid tag ref",
			cloneUrl:    cloneUrl,
			privateRepo: false,
			branchRef:   invalidTagRef,
			expectedRef: "",
			expectedErr: ErrInvalidReference,
		},
		{
			name:        "Private Repository",
			cloneUrl:    "https://github.com/kimdre/doco-cd_tests.git",
			privateRepo: true,
			branchRef:   "destroy",
			expectedRef: "refs/heads/destroy",
			expectedErr: nil,
		},
	}

	encryption.SetupAgeKeyEnvVar(t)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := config.GetAppConfig()
			if err != nil {
				t.Fatalf("Failed To get app config: %v", err)
			}

			if tc.privateRepo {
				tc.cloneUrl = GetAuthUrl(
					tc.cloneUrl,
					c.AuthType,
					c.GitAccessToken,
				)
			}

			repo, err := CloneRepository(t.TempDir(), tc.cloneUrl, MainBranch, false, c.HttpProxy, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase)
			if err != nil {
				t.Fatalf("Failed To clone repository: %v", err)
			}

			if repo == nil {
				t.Fatal("Repository is nil")
			}

			worktree, err := repo.Worktree()
			if err != nil {
				t.Fatalf("Failed To get worktree: %v", err)
			}

			repo, err = UpdateRepository(worktree.Filesystem.Root(), tc.cloneUrl, tc.branchRef, true, c.HttpProxy, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase)
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
					t.Fatalf("Failed To get worktree: %v", err)
				}
			}

			refName := plumbing.ReferenceName(tc.expectedRef)
			if tc.expectedRef != "" {
				ref, err := repo.Reference(refName, true)
				if err != nil {
					t.Fatalf("Failed To get reference: %v", err)
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

func TestGetReferenceSet(t *testing.T) {
	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed To get app config: %v", err)
	}

	repo, err := CloneRepository(t.TempDir(), cloneUrl, MainBranch, false, c.HttpProxy, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase)
	if err != nil {
		t.Fatalf("Failed To clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil")
	}

	refSet, err := GetReferenceSet(repo, MainBranch)
	if err != nil {
		t.Fatalf("Failed To get reference set: %v", err)
	}

	if refSet.localRef == "" || refSet.remoteRef == "" {
		t.Fatal("Reference set is incomplete")
	}

	if refSet.localRef.String() != MainBranch {
		t.Fatalf("Expected local reference %s, got %s", MainBranch, refSet.localRef.String())
	}

	if refSet.remoteRef.String() != remoteMainBranch {
		t.Fatalf("Expected remote reference %s, got %s", remoteMainBranch, refSet.remoteRef.String())
	}
}

func TestUpdateRepository_KeepUntrackedFiles(t *testing.T) {
	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed To get app config: %v", err)
	}

	url := GetAuthUrl(cloneUrlTest, c.AuthType, c.GitAccessToken)

	repo, err := CloneRepository(t.TempDir(), url, MainBranch, false, c.HttpProxy, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase)
	if err != nil {
		t.Fatalf("Failed To clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil")
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed To get worktree: %v", err)
	}

	// Add a new file To the cloned repository
	newFileName := "new.txt"

	_, err = worktree.Filesystem.Create(newFileName)
	if err != nil {
		t.Fatalf("Failed To create new file: %v", err)
	}

	repo, err = UpdateRepository(worktree.Filesystem.Root(), url, "alternative", true, c.HttpProxy, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase)
	if err != nil {
		t.Fatalf("Failed To update repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil after update")
	}

	files, err := worktree.Filesystem.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed To read directory: %v", err)
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
	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed To get app config: %v", err)
	}

	repo, err := CloneRepository(t.TempDir(), cloneUrl, MainBranch, false, c.HttpProxy, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase)
	if err != nil {
		t.Fatalf("Failed To clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil")
	}

	commit, err := GetLatestCommit(repo, MainBranch)
	if err != nil {
		t.Fatalf("Failed To get latest commit: %v", err)
	}

	if commit == "" {
		t.Fatal("Commit hash is empty")
	}

	t.Log(commit)
}

func TestGetChangedFilesBetweenCommits(t *testing.T) {
	var (
		commitOld                = plumbing.NewHash("f8c5992297bf70eb01f0ba40d062896b1f48dc65")
		commitNew                = plumbing.NewHash("e72ef851774e50b82c173fd36cfcf9a88355c592")
		expectedChangedDirectory = "html"
		expectedChangedFile      = filepath.Join(expectedChangedDirectory, "index.html")
	)

	tmpDir := t.TempDir()

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed To get app config: %v", err)
	}

	url := GetAuthUrl(cloneUrlTest, c.AuthType, c.GitAccessToken)

	repo, err := CloneRepository(tmpDir, url, MainBranch, false, c.HttpProxy, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase)
	if err != nil {
		t.Fatalf("Failed To clone repository: %v", err)
	}

	changedFiles, err := GetChangedFilesBetweenCommits(repo, commitOld, commitNew)
	if err != nil {
		t.Fatalf("Failed To get changed files: %v", err)
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

	hasChanged, err := HasChangesInSubdir(changedFiles, tmpDir, expectedChangedDirectory)
	if err != nil {
		t.Fatalf("Failed To check changes in subdir: %v", err)
	}

	if !hasChanged {
		t.Errorf("Expected changes in subdir %s, but found none", expectedChangedDirectory)
	}
}

func TestSSHAuth(t *testing.T) {
	const (
		encryptedKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAACmFlczI1Ni1jdHIAAAAGYmNyeXB0AAAAGAAAABA+Zz/91P
rp2u7NvTWBtLI0AAAAGAAAAAEAAAAzAAAAC3NzaC1lZDI1NTE5AAAAIFyEIiKcYAJl82Ga
40hVJoKO1qOvVfekORkGLSsKFnF7AAAAoBgOn6fvoLqNvcj0QMyuZTYVJEm9YXs8zNkG+9
suGsdNHOvMRQWLzq9VJiJUyOG29zayIQ4Q3pZlcoRINpUI9yl4/eFza7P4MEHDVBLF531K
X3nAnZomTg2czfus92AmR+3kYDWvBE1WkpieAaRfVTuBtNcB41rOAZMLQ001zhVF2qdb+D
+tvLTkrbIyLPEbZOBHuCH+mVgPefYCRXsB9Nw=
-----END OPENSSH PRIVATE KEY-----`
		encryptedKeyPassphrase = "doco-cd"
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
			auth, err := sshAuth(tc.privateKey, tc.passphrase)
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
			result := convertSSHUrl(tc.sshUrl)
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
