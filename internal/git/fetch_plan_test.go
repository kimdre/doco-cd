package git

import (
	"errors"
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

	if err := FetchRepositoryReference(cloneRepo, originPath, "v1.0.0", false, transport.ProxyOptions{}, nil, 0); err != nil {
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

	if err := FetchRepositoryReference(cloneRepo, originPath, "v1.0.0", false, transport.ProxyOptions{}, nil, 0); err != nil {
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

func TestFetchRepositoryReferencePrunesStaleRequestedBranch(t *testing.T) {
	t.Parallel()

	originPath, clonePath, originHash := setupLocalMainRepoAndClone(t)

	originRepo, err := git.PlainOpen(originPath)
	if err != nil {
		t.Fatalf("open origin: %v", err)
	}

	staleBranch := plumbing.NewBranchReferenceName("release")
	requestedTag := plumbing.NewTagReferenceName("release")
	unrelatedTag := plumbing.NewTagReferenceName("unrelated")

	for _, name := range []plumbing.ReferenceName{staleBranch, requestedTag, unrelatedTag} {
		if err := originRepo.Storer.SetReference(plumbing.NewHashReference(name, originHash)); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	cloneRepo, err := git.PlainOpen(clonePath)
	if err != nil {
		t.Fatalf("open clone: %v", err)
	}

	if err := FetchRepository(cloneRepo, originPath, false, transport.ProxyOptions{}, nil, 0); err != nil {
		t.Fatalf("fetch initial references: %v", err)
	}

	upstreamRef := plumbing.NewRemoteReferenceName("upstream", staleBranch.Short())
	if err := cloneRepo.Storer.SetReference(plumbing.NewHashReference(upstreamRef, originHash)); err != nil {
		t.Fatalf("create upstream reference: %v", err)
	}

	// Only the requested branch disappears upstream; the unrelated tag stays
	// behind in the clone because a focused fetch never inspects it.
	if err := originRepo.Storer.RemoveReference(staleBranch); err != nil {
		t.Fatalf("delete stale branch: %v", err)
	}

	if err := originRepo.Storer.RemoveReference(unrelatedTag); err != nil {
		t.Fatalf("delete unrelated tag: %v", err)
	}

	if err := FetchRepositoryReference(cloneRepo, originPath, staleBranch.Short(), false, transport.ProxyOptions{}, nil, 0); err != nil {
		t.Fatalf("focused fetch: %v", err)
	}

	remoteStaleBranch := plumbing.NewRemoteReferenceName(RemoteName, staleBranch.Short())
	if _, err := cloneRepo.Reference(remoteStaleBranch, false); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Fatalf("stale reference %s was not pruned: %v", remoteStaleBranch, err)
	}

	for _, name := range []plumbing.ReferenceName{requestedTag, unrelatedTag, upstreamRef} {
		if _, err := cloneRepo.Reference(name, false); err != nil {
			t.Fatalf("reference %s must not be pruned: %v", name, err)
		}
	}
}
