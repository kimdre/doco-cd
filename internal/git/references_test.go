package git

import (
	"errors"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// TestGetReferenceSet_LocalResolution exercises every resolution path of
// GetReferenceSet using a fully local origin/clone pair, without requiring
// network access. Covered cases:
//
//   - short branch name         → refs/heads/<name> with remote tracking ref
//   - full refs/heads/...       → same as above
//   - refs/remotes/origin/...   → returned directly
//   - short tag name            → refs/tags/<name>
//   - full refs/tags/...        → same
//   - commit SHA                → returned as-is; RemoteRef is empty, RemoteHash is zero
//   - remote-only tracking ref  → resolves via refs/remotes/origin/<name> only
//   - non-existent refs         → ErrInvalidReference
func TestGetReferenceSet_LocalResolution(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	originPath := filepath.Join(base, "origin")
	clonePath := filepath.Join(base, "clone")

	originRepo, err := gogit.PlainInit(originPath, false)
	if err != nil {
		t.Fatalf("init origin: %v", err)
	}

	mainHash := commitFile(t, originRepo, originPath, "README.md", "initial\n", "initial commit")

	for _, ref := range []plumbing.Reference{
		*plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), mainHash),
		*plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main")),
		*plumbing.NewHashReference(plumbing.NewTagReferenceName("v1.0.0"), mainHash),
	} {
		if err := originRepo.Storer.SetReference(&ref); err != nil {
			t.Fatalf("set %s: %v", ref.Name(), err)
		}
	}

	_ = originRepo.Storer.RemoveReference(plumbing.NewBranchReferenceName("master"))

	// Checkout feature/foo before committing so that commitFile advances
	// refs/heads/feature/foo, not refs/heads/main (which HEAD currently tracks).
	originWt, err := originRepo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	if err := originWt.Checkout(&gogit.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("feature/foo"),
		Create: true,
		Keep:   true,
	}); err != nil {
		t.Fatalf("checkout feature/foo: %v", err)
	}

	featureHash := commitFile(t, originRepo, originPath, "feature.txt", "feature\n", "feature commit")

	clonedRepo, err := CloneRepository(clonePath, originPath, MainBranch, false, transport.ProxyOptions{}, nil, false, 0)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// go-git PlainClone creates local tracking branches for every remote branch,
	// so refs/heads/feature/foo also exists in the clone. To test the remote-only
	// path we manually inject a tracking ref that has no local counterpart.
	remoteOnlyHash := featureHash
	if err := clonedRepo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName(RemoteName, "remote-only"), remoteOnlyHash),
	); err != nil {
		t.Fatalf("inject remote-only ref: %v", err)
	}

	remotePrefix := "refs/remotes/" + RemoteName + "/"

	tests := []struct {
		ref            string
		wantLocalRef   string
		wantRemoteRef  string
		wantRemoteHash plumbing.Hash // zero means "don't care / expect ZeroHash"
		wantErr        error
	}{
		// Short branch name resolves to the local tracking branch first.
		{
			ref:            "main",
			wantLocalRef:   BranchPrefix + "main",
			wantRemoteRef:  remotePrefix + "main",
			wantRemoteHash: mainHash,
		},
		// Full refs/heads/ name uses the same resolution as the short form.
		{
			ref:            BranchPrefix + "main",
			wantLocalRef:   BranchPrefix + "main",
			wantRemoteRef:  remotePrefix + "main",
			wantRemoteHash: mainHash,
		},
		// Remote tracking ref passed in full is resolved without local branch lookup.
		{
			ref:            remotePrefix + "main",
			wantLocalRef:   remotePrefix + "main",
			wantRemoteRef:  remotePrefix + "main",
			wantRemoteHash: mainHash,
		},
		// Short tag name resolves after branch candidates fail.
		{
			ref:            "v1.0.0",
			wantLocalRef:   TagPrefix + "v1.0.0",
			wantRemoteRef:  TagPrefix + "v1.0.0",
			wantRemoteHash: mainHash,
		},
		// Full tag ref passed directly.
		{
			ref:            TagPrefix + "v1.0.0",
			wantLocalRef:   TagPrefix + "v1.0.0",
			wantRemoteRef:  TagPrefix + "v1.0.0",
			wantRemoteHash: mainHash,
		},
		// Commit SHA is returned directly; no reference store lookup.
		{
			ref:           mainHash.String(),
			wantLocalRef:  mainHash.String(),
			wantRemoteRef: "",
			// RemoteHash stays ZeroHash for commit SHA refs.
		},
		// go-git PlainClone creates refs/heads/feature/foo locally too (unlike
		// standard git), so "feature/foo" resolves to the local tracking branch.
		{
			ref:            "feature/foo",
			wantLocalRef:   BranchPrefix + "feature/foo",
			wantRemoteRef:  remotePrefix + "feature/foo",
			wantRemoteHash: featureHash,
		},
		// Full remote tracking ref for that branch.
		{
			ref:            remotePrefix + "feature/foo",
			wantLocalRef:   remotePrefix + "feature/foo",
			wantRemoteRef:  remotePrefix + "feature/foo",
			wantRemoteHash: featureHash,
		},
		// A tracking ref with no local refs/heads counterpart resolves via
		// the remote-tracking candidate (second in the short-name list).
		{
			ref:            "remote-only",
			wantLocalRef:   remotePrefix + "remote-only",
			wantRemoteRef:  remotePrefix + "remote-only",
			wantRemoteHash: remoteOnlyHash,
		},
		// Non-existent refs must yield ErrInvalidReference, not a storage error.
		{ref: "no-such-branch", wantErr: ErrInvalidReference},
		{ref: BranchPrefix + "no-such", wantErr: ErrInvalidReference},
		{ref: TagPrefix + "no-such", wantErr: ErrInvalidReference},
		{ref: remotePrefix + "no-such", wantErr: ErrInvalidReference},
	}

	for _, tc := range tests {
		t.Run(tc.ref, func(t *testing.T) {
			refSet, err := GetReferenceSet(clonedRepo, tc.ref)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected error %v, got: %v", tc.wantErr, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if refSet.LocalRef.String() != tc.wantLocalRef {
				t.Errorf("LocalRef:    got %q, want %q", refSet.LocalRef, tc.wantLocalRef)
			}

			if refSet.RemoteRef.String() != tc.wantRemoteRef {
				t.Errorf("RemoteRef:   got %q, want %q", refSet.RemoteRef, tc.wantRemoteRef)
			}

			if tc.wantRemoteHash != plumbing.ZeroHash && refSet.RemoteHash != tc.wantRemoteHash {
				t.Errorf("RemoteHash:  got %s, want %s", refSet.RemoteHash, tc.wantRemoteHash)
			}

			if tc.wantRemoteHash == plumbing.ZeroHash && refSet.RemoteHash != plumbing.ZeroHash {
				t.Errorf("RemoteHash:  expected zero hash, got %s", refSet.RemoteHash)
			}
		})
	}
}

// TestGetReferenceSet_UnsafeNames verifies that reference names that cannot be
// stored safely (escaping, empty, containing a backslash, or single-level
// non-pseudo-ref names) return ErrInvalidReference rather than surfacing the
// go-git storage-layer validation error. Callers treat non-ErrInvalidReference
// errors as transient failures and would otherwise retry or trigger a repair.
func TestGetReferenceSet_UnsafeNames(t *testing.T) {
	repo, err := gogit.PlainInit(t.TempDir(), false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	unsafeRefs := []string{
		"",
		"..",
		"../../config",
		"refs/heads/..",
		"refs/heads/../../config",
		"refs/",
		"a\\b",
		"config",
	}

	for _, ref := range unsafeRefs {
		t.Run(ref, func(t *testing.T) {
			_, err := GetReferenceSet(repo, ref)
			if !errors.Is(err, ErrInvalidReference) {
				t.Fatalf("expected ErrInvalidReference for %q, got: %v", ref, err)
			}
		})
	}
}
