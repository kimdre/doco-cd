package git

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/config"
)

const (
	cloneUrl            = "https://github.com/kimdre/doco-cd.git"
	validBranchRef      = MainBranch
	validBranchRefShort = "main"
	validTagRef         = "refs/tags/v0.15.0"
	validTagRefShort    = "v0.15.0"
	invalidRef          = "refs/heads/invalid"
	invalidTagRef       = "refs/tags/invalid"
	validCommitSHA      = "903b270da7505fe8b13b42d3b191b08fb9ca3247"
	invalidCommitSHA    = "1111111111111111111111111111111111111111"
)

func TestGetAuthUrl(t *testing.T) {
	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
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
	repo, err := CloneRepository(t.TempDir(), cloneUrl, validBranchRef, false)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Repository is nil")
	}

	// Check files in the repository
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

	// Check if the repository is cloned
	if worktree.Filesystem.Root() == "" {
		t.Fatal("Repository is not cloned")
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
			expectedRef: validBranchRef,
			expectedErr: nil,
		},
		{
			name:        "Valid short branch ref",
			cloneUrl:    cloneUrl,
			branchRef:   validBranchRefShort,
			expectedRef: validBranchRef,
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
			expectedErr: ErrCheckoutFailed,
		},
		{
			name:        "Invalid tag ref",
			cloneUrl:    cloneUrl,
			privateRepo: false,
			branchRef:   invalidTagRef,
			expectedRef: "",
			expectedErr: ErrCheckoutFailed,
		},
		{
			name:        "Private Repository",
			cloneUrl:    "https://github.com/kimdre/test.git",
			privateRepo: true,
			branchRef:   "destroy",
			expectedRef: "refs/heads/destroy",
			expectedErr: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.privateRepo {
				c, err := config.GetAppConfig()
				if err != nil {
					t.Fatalf("Failed to get app config: %v", err)
				}

				tc.cloneUrl = GetAuthUrl(
					tc.cloneUrl,
					c.AuthType,
					c.GitAccessToken,
				)
			}

			repo, err := CloneRepository(t.TempDir(), tc.cloneUrl, MainBranch, false)
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

			repo, err = UpdateRepository(worktree.Filesystem.Root(), tc.branchRef, true)
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

			refName := plumbing.ReferenceName(tc.expectedRef)
			if tc.expectedRef != "" {
				ref, err := repo.Reference(refName, true)
				if err != nil {
					t.Fatalf("Failed to get reference: %v", err)
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
