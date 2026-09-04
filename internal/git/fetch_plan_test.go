package git

import (
	"os"
	"reflect"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

func TestFocusedFetchRefSpecs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  string
		want [][]config.RefSpec
	}{
		{
			name: "fully qualified branch",
			ref:  "refs/heads/feature/test",
			want: [][]config.RefSpec{{"+refs/heads/feature/test:refs/remotes/origin/feature/test"}},
		},
		{
			name: "fully qualified tag",
			ref:  "refs/tags/v1.2.3",
			want: [][]config.RefSpec{{"+refs/tags/v1.2.3:refs/tags/v1.2.3"}},
		},
		{
			name: "short name keeps branch before tag",
			ref:  "release",
			want: [][]config.RefSpec{
				{"+refs/heads/release:refs/remotes/origin/release"},
				{"+refs/tags/release:refs/tags/release"},
			},
		},
		{name: "commit hash uses compatibility fetch", ref: "0123456789012345678901234567890123456789"},
		{name: "pseudo ref uses compatibility fetch", ref: plumbing.HEAD.String()},
		{name: "unsupported qualified ref uses compatibility fetch", ref: "refs/changes/1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := focusedFetchRefSpecs(tt.ref); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("focusedFetchRefSpecs(%q) = %v, want %v", tt.ref, got, tt.want)
			}
		})
	}
}

func TestFetchReferenceRepository_ResolvesShortTag(t *testing.T) {
	t.Parallel()

	originPath, clonePath, originHash := setupLocalMainRepoAndClone(t)

	originRepo, err := git.PlainOpen(originPath)
	if err != nil {
		t.Fatalf("open origin: %v", err)
	}

	if err := originRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName("v1.0.0"), originHash)); err != nil {
		t.Fatalf("create tag: %v", err)
	}

	cloneRepo, err := git.PlainOpen(clonePath)
	if err != nil {
		t.Fatalf("open clone: %v", err)
	}

	if err := FetchReferenceRepository(cloneRepo, originPath, "v1.0.0", false, transport.ProxyOptions{}, nil, 0); err != nil {
		t.Fatalf("fetch short tag: %v", err)
	}

	tag, err := cloneRepo.Reference(plumbing.NewTagReferenceName("v1.0.0"), true)
	if err != nil {
		t.Fatalf("read fetched tag: %v", err)
	}

	if tag.Hash() != originHash {
		t.Fatalf("tag hash = %s, want %s", tag.Hash(), originHash)
	}

	newHash := commitFile(t, originRepo, originPath, "updated.md", "updated\n", "updated")
	if err := originRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName("v1.0.0"), newHash)); err != nil {
		t.Fatalf("move tag: %v", err)
	}

	if err := FetchReferenceRepository(cloneRepo, originPath, "v1.0.0", false, transport.ProxyOptions{}, nil, 0); err != nil {
		t.Fatalf("refresh short tag: %v", err)
	}

	tag, err = cloneRepo.Reference(plumbing.NewTagReferenceName("v1.0.0"), true)
	if err != nil {
		t.Fatalf("read refreshed tag: %v", err)
	}

	if tag.Hash() != newHash {
		t.Fatalf("refreshed tag hash = %s, want %s", tag.Hash(), newHash)
	}
}

func TestSyncRepositoryReportsCloneUpdateAndCurrent(t *testing.T) {
	t.Parallel()

	originPath, clonePath, originalHash := setupLocalMainRepoAndClone(t)
	if err := os.RemoveAll(clonePath); err != nil {
		t.Fatalf("remove initial clone: %v", err)
	}

	result, err := SyncRepository(clonePath, originPath, MainBranch, false, transport.ProxyOptions{}, nil, false, 0)
	if err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	if result.State != SyncStateCloned {
		t.Fatalf("initial sync state = %q, want %q", result.State, SyncStateCloned)
	}

	assertRepoOnMainHash(t, result.Repository, originalHash)

	originRepo, err := git.PlainOpen(originPath)
	if err != nil {
		t.Fatalf("open origin: %v", err)
	}

	newHash := commitFile(t, originRepo, originPath, "updated.md", "updated\n", "updated")

	result, err = SyncRepository(clonePath, originPath, MainBranch, false, transport.ProxyOptions{}, nil, false, 0)
	if err != nil {
		t.Fatalf("updated sync: %v", err)
	}

	if result.State != SyncStateUpdated {
		t.Fatalf("updated sync state = %q, want %q", result.State, SyncStateUpdated)
	}

	assertRepoOnMainHash(t, result.Repository, newHash)

	result, err = SyncRepository(clonePath, originPath, MainBranch, false, transport.ProxyOptions{}, nil, false, 0)
	if err != nil {
		t.Fatalf("current sync: %v", err)
	}

	if result.State != SyncStateCurrent {
		t.Fatalf("current sync state = %q, want %q", result.State, SyncStateCurrent)
	}
}

func TestSyncRepositorySwitchesRequestedReferences(t *testing.T) {
	t.Parallel()

	originPath, clonePath, mainHash := setupLocalMainRepoAndClone(t)

	originRepo, err := git.PlainOpen(originPath)
	if err != nil {
		t.Fatalf("open origin: %v", err)
	}

	worktree, err := originRepo.Worktree()
	if err != nil {
		t.Fatalf("origin worktree: %v", err)
	}

	if err := worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("feature"),
		Create: true,
		Keep:   true,
	}); err != nil {
		t.Fatalf("create feature branch: %v", err)
	}

	featureHash := commitFile(t, originRepo, originPath, "feature.md", "feature\n", "feature")

	result, err := SyncRepository(clonePath, originPath, "feature", false, transport.ProxyOptions{}, nil, false, 0)
	if err != nil {
		t.Fatalf("sync feature branch: %v", err)
	}

	if result.State != SyncStateUpdated {
		t.Fatalf("feature sync state = %q, want %q", result.State, SyncStateUpdated)
	}

	head, err := result.Repository.Head()
	if err != nil {
		t.Fatalf("feature HEAD: %v", err)
	}

	if head.Hash() != featureHash {
		t.Fatalf("feature HEAD = %s, want %s", head.Hash(), featureHash)
	}

	result, err = SyncRepository(clonePath, originPath, MainBranch, false, transport.ProxyOptions{}, nil, false, 0)
	if err != nil {
		t.Fatalf("sync main branch: %v", err)
	}

	head, err = result.Repository.Head()
	if err != nil {
		t.Fatalf("main HEAD: %v", err)
	}

	if head.Hash() != mainHash {
		t.Fatalf("main HEAD = %s, want %s", head.Hash(), mainHash)
	}
}
