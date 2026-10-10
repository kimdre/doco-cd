package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitfs "github.com/go-git/go-git/v5/storage/filesystem"
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

func TestFetchPinnedCommitErrorClassification(t *testing.T) {
	t.Parallel()

	repo, err := git.PlainInit(t.TempDir(), true)
	if err != nil {
		t.Fatalf("initialize bare repository: %v", err)
	}

	const sha = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	tests := []struct {
		name     string
		fetchErr error
		wantErr  bool
	}{
		{name: "SHA fetch unsupported", fetchErr: git.ErrExactSHA1NotSupported},
		{name: "remote object missing", fetchErr: fmt.Errorf("remote: %w", plumbing.ErrObjectNotFound)},
		{name: "authentication failure", fetchErr: transport.ErrAuthenticationRequired, wantErr: true},
		{name: "transport failure", fetchErr: errors.New("connection closed during fetch"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			err := fetchPinnedCommit(repo, sha, func(refSpec config.RefSpec) error {
				called = true

				if want := config.RefSpec("+" + sha + ":refs/doco-cd/pinned/" + sha); refSpec != want {
					t.Errorf("refSpec = %q, want %q", refSpec, want)
				}

				return tt.fetchErr
			})

			if !called {
				t.Fatal("fetchPinnedCommit did not attempt to fetch the missing SHA")
			}

			if tt.wantErr && !errors.Is(err, tt.fetchErr) {
				t.Fatalf("fetchPinnedCommit() error = %v, want %v", err, tt.fetchErr)
			}

			if !tt.wantErr && err != nil {
				t.Fatalf("fetchPinnedCommit() error = %v, want nil for missing SHA", err)
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

func TestFetchRepositoryReferenceNamespaceCollision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		old       []string
		requested string
		tag       bool
		qualified bool
		broad     bool
		packed    bool
		empty     bool
		annotated bool
	}{
		{name: "branch parent", old: []string{"release/old"}, requested: "release"},
		{name: "qualified branch parent", old: []string{"release/old"}, requested: "release", qualified: true},
		{name: "branch child", old: []string{"release"}, requested: "release/new"},
		{name: "qualified branch child", old: []string{"release"}, requested: "release/new", qualified: true},
		{name: "deep ancestor", old: []string{"release"}, requested: "release/new/deep"},
		{name: "multiple descendants", old: []string{"release/old/deep", "release/other"}, requested: "release"},
		{name: "empty directory", old: []string{"release/old"}, requested: "release", empty: true},
		{name: "packed descendants", old: []string{"release/old"}, requested: "release", packed: true},
		{name: "nonblocking packed ancestor", old: []string{"release"}, requested: "release/new", packed: true},
		{name: "broad branch parent", old: []string{"release/old"}, requested: "release", broad: true},
		{name: "broad branch child", old: []string{"release"}, requested: "release/new", broad: true},
		{name: "tag parent", old: []string{"release/old"}, requested: "release", tag: true},
		{name: "qualified tag parent", old: []string{"release/old"}, requested: "release", tag: true, qualified: true},
		{name: "tag child", old: []string{"release"}, requested: "release/new", tag: true},
		{name: "qualified tag child", old: []string{"release"}, requested: "release/new", tag: true, qualified: true},
		{name: "packed tags", old: []string{"release/old"}, requested: "release", tag: true, packed: true},
		{name: "broad tag parent", old: []string{"release/old"}, requested: "release", tag: true, broad: true},
		{name: "annotated tag parent", old: []string{"release/old"}, requested: "release", tag: true, annotated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			originPath, clonePath, oldHash := setupLocalMainRepoAndClone(t)

			origin, err := git.PlainOpen(originPath)
			if err != nil {
				t.Fatal(err)
			}

			repo, err := git.PlainOpen(clonePath)
			if err != nil {
				t.Fatal(err)
			}

			sourcePrefix, destinationPrefix := BranchPrefix, "refs/remotes/origin/"
			if tt.tag {
				sourcePrefix, destinationPrefix = TagPrefix, TagPrefix
			}

			var oldNames []plumbing.ReferenceName

			for _, old := range tt.old {
				name := plumbing.ReferenceName(sourcePrefix + old)

				oldNames = append(oldNames, name)
				if err := origin.Storer.SetReference(plumbing.NewHashReference(name, oldHash)); err != nil {
					t.Fatal(err)
				}
			}

			preserved := []plumbing.ReferenceName{
				"refs/heads/local", "refs/tags/unrelated", "refs/remotes/upstream/release", "refs/remotes/origin/keep",
			}
			for _, name := range preserved {
				if err := repo.Storer.SetReference(plumbing.NewHashReference(name, oldHash)); err != nil {
					t.Fatal(err)
				}

				for _, name := range []plumbing.ReferenceName{"refs/tags/unrelated", "refs/heads/keep"} {
					if err := origin.Storer.SetReference(plumbing.NewHashReference(name, oldHash)); err != nil {
						t.Fatal(err)
					}
				}
			}

			if !tt.tag {
				if err := origin.Storer.SetReference(plumbing.NewHashReference(
					plumbing.NewTagReferenceName(tt.requested), oldHash)); err != nil {
					t.Fatal(err)
				}
			}

			if err := FetchRepository(repo, originPath, false, transport.ProxyOptions{}, nil, 0); err != nil {
				t.Fatal(err)
			}

			if tt.packed {
				storage, ok := repo.Storer.(*gitfs.Storage)
				if !ok {
					t.Fatal("expected filesystem storage")
				}

				if err := storage.PackRefs(); err != nil {
					t.Fatal(err)
				}
			}

			if tt.empty {
				for _, old := range tt.old {
					if err := repo.Storer.RemoveReference(plumbing.ReferenceName(destinationPrefix + old)); err != nil {
						t.Fatal(err)
					}
				}
			}

			removeFixtureReferences(t, origin, filepath.Join(originPath, ".git"), oldNames)
			newHash := commitFile(t, origin, originPath, "new.md", "new revision\n", "new revision")
			requested := plumbing.ReferenceName(sourcePrefix + tt.requested)

			if tt.annotated {
				if _, err := origin.CreateTag(tt.requested, newHash, &git.CreateTagOptions{
					Tagger:  &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
					Message: "replacement tag",
				}); err != nil {
					t.Fatal(err)
				}
			} else if err := origin.Storer.SetReference(plumbing.NewHashReference(requested, newHash)); err != nil {
				t.Fatal(err)
			}

			ref := tt.requested
			if tt.qualified {
				ref = requested.String()
			}

			if tt.broad {
				err = FetchRepository(repo, originPath, false, transport.ProxyOptions{}, nil, 0)
			} else {
				err = FetchRepositoryReference(repo, originPath, ref, false, transport.ProxyOptions{}, nil, 0)
			}

			if err != nil {
				t.Fatalf("fetch namespace transition: %v", err)
			}

			got, err := ResolveReferenceCommit(repo, ref)
			if err != nil || got != newHash {
				t.Fatalf("resolved commit = %s, %v; want %s", got, err, newHash)
			}

			for _, name := range preserved {
				got, err := repo.Reference(name, false)
				if err != nil || got.Hash() != oldHash {
					t.Fatalf("preserved ref %s = %v, %v", name, got, err)
				}
			}
		})
	}
}

func removeFixtureReferences(t *testing.T, repo *git.Repository, gitDir string, names []plumbing.ReferenceName) {
	t.Helper()

	for _, name := range names {
		if err := repo.Storer.RemoveReference(name); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range names {
		root := filepath.Join(gitDir, "refs", "heads")
		if name.IsTag() {
			root = filepath.Join(gitDir, "refs", "tags")
		}

		for dir := filepath.Dir(filepath.Join(gitDir, name.String())); dir != root; dir = filepath.Dir(dir) {
			if err := os.Remove(dir); err != nil {
				if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, os.ErrNotExist) {
					break
				}

				t.Fatal(err)
			}
		}
	}
}

func TestFetchRepositoryReferenceNamespaceCollisionUsesURLOverride(t *testing.T) {
	t.Parallel()

	oldPath, clonePath, oldHash := setupLocalMainRepoAndClone(t)

	oldOrigin, err := git.PlainOpen(oldPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := oldOrigin.Storer.SetReference(plumbing.NewHashReference("refs/heads/release/old", oldHash)); err != nil {
		t.Fatal(err)
	}

	repo, err := git.PlainOpen(clonePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := FetchRepository(repo, oldPath, false, transport.ProxyOptions{}, nil, 0); err != nil {
		t.Fatal(err)
	}

	// Share history so the in-process server recognizes the clone's negotiation haves.
	newPath := filepath.Join(t.TempDir(), "replacement-origin")

	newOrigin, err := git.PlainClone(newPath, false, &git.CloneOptions{
		URL: oldPath, ReferenceName: plumbing.NewBranchReferenceName("main"),
	})
	if err != nil {
		t.Fatal(err)
	}

	hash := commitFile(t, newOrigin, newPath, "replacement.md", "replacement\n", "replacement")
	if err := newOrigin.Storer.SetReference(plumbing.NewHashReference("refs/heads/release", hash)); err != nil {
		t.Fatal(err)
	}

	if err := FetchRepositoryReference(repo, newPath, "release", false, transport.ProxyOptions{}, nil, 0); err != nil {
		t.Fatal(err)
	}

	if got, err := ResolveReferenceCommit(repo, "release"); err != nil || got != hash {
		t.Fatalf("resolved = %s, %v; want %s", got, err, hash)
	}

	remote, err := repo.Remote(RemoteName)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(remote.Config().URLs, []string{oldPath}) {
		t.Fatalf("discovery changed persisted remote URL: %v", remote.Config().URLs)
	}
}
