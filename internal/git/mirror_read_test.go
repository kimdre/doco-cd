package git

import (
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// setupMirrorWithPack creates a local origin repository and a bare mirror clone of
// it, and returns the origin path, the mirror path and the origin's current main
// commit. The mirror's objects live in a packfile, which is what makes it a
// faithful stand-in for the bare mirrors GitStore maintains.
func setupMirrorWithPack(t *testing.T) (originPath, mirrorPath string, mainHash plumbing.Hash) {
	t.Helper()

	originPath, _, mainHash = setupLocalMainRepoAndClone(t)
	mirrorPath = filepath.Join(t.TempDir(), "mirror")

	if _, err := gogit.PlainClone(mirrorPath, true, &gogit.CloneOptions{
		URL:        originPath,
		RemoteName: RemoteName,
	}); err != nil {
		t.Fatalf("clone bare mirror: %v", err)
	}

	return originPath, mirrorPath, mainHash
}

// fetchNewCommitIntoMirror adds a commit to origin and fetches it into the mirror.
// go-git writes every fetched object set as a new packfile, so this reproduces the
// event that invalidates an already-opened repository handle.
func fetchNewCommitIntoMirror(t *testing.T, originPath, mirrorPath string, content, msg string) plumbing.Hash {
	t.Helper()

	originRepo, err := gogit.PlainOpen(originPath)
	if err != nil {
		t.Fatalf("open origin: %v", err)
	}

	newHash := commitFile(t, originRepo, originPath, "README.md", content, msg)

	if err := originRepo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), newHash),
	); err != nil {
		t.Fatalf("advance origin main: %v", err)
	}

	mirrorRepo, err := gogit.PlainOpen(mirrorPath)
	if err != nil {
		t.Fatalf("open mirror: %v", err)
	}

	if err := FetchRepositoryReference(mirrorRepo, originPath, MainBranch, false, transport.ProxyOptions{}, nil, 0); err != nil {
		t.Fatalf("fetch into mirror: %v", err)
	}

	return newHash
}

// TestWithMirrorRead_SeesPackfilesWrittenAfterAnEarlierRead is the regression test
// for the post-deploy panic in GetShortestUniqueCommitHash.
//
// go-git's filesystem.ObjectStorage builds its packfile index map lazily and then
// caches it for the lifetime of the repository handle, while dotgit.ObjectPacks
// re-reads the pack directory on every call. A handle that read objects before
// another job fetched therefore enumerates a packfile it holds no index for, and
// dereferences that missing index as a nil idxfile.Index inside
// packfile.GetByType - a segfault rather than an error.
//
// WithMirrorRead must be immune to that by opening a fresh handle per locked read
// region, so a commit that only exists in a packfile written after an earlier read
// still resolves.
func TestWithMirrorRead_SeesPackfilesWrittenAfterAnEarlierRead(t *testing.T) {
	originPath, mirrorPath, mainHash := setupMirrorWithPack(t)

	// First read region: populates go-git's cached packfile index for this mirror.
	if err := WithMirrorRead(mirrorPath, func(repo *gogit.Repository) error {
		_, err := GetShortestUniqueCommitHash(repo, mainHash.String(), DefaultShortSHALength)

		return err
	}); err != nil {
		t.Fatalf("initial mirror read: %v", err)
	}

	newHash := fetchNewCommitIntoMirror(t, originPath, mirrorPath, "second\n", "second commit")

	// Second read region: the commit lives only in the packfile written by the
	// fetch above. A cached handle would panic here; a fresh one must resolve it.
	short, err := MirrorRead(mirrorPath, func(repo *gogit.Repository) (string, error) {
		return GetShortestUniqueCommitHash(repo, newHash.String(), DefaultShortSHALength)
	})
	if err != nil {
		t.Fatalf("mirror read after fetch: %v", err)
	}

	if want := newHash.String()[:DefaultShortSHALength]; short != want {
		t.Fatalf("GetShortestUniqueCommitHash = %q, want %q", short, want)
	}
}

func TestWithMirrorRead_RejectsEmptyMirrorDir(t *testing.T) {
	t.Parallel()

	called := false

	err := WithMirrorRead("", func(*gogit.Repository) error {
		called = true

		return nil
	})
	if err == nil {
		t.Fatal("expected an error for an empty mirror directory")
	}

	if called {
		t.Fatal("callback must not run without a mirror directory")
	}
}

func TestMirrorRead_RejectsNilCallback(t *testing.T) {
	t.Parallel()

	result, err := MirrorRead[string]("/unused", nil)
	if err == nil {
		t.Fatal("MirrorRead() error = nil, want an error")
	}

	if result != "" {
		t.Fatalf("MirrorRead() result = %q, want the zero value", result)
	}
}
