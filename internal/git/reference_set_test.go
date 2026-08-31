package git_test

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/git"
)

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
