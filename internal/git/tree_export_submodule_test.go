package git_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/git"
)

// storeTestObject encodes obj into repo's object database and returns its hash.
func storeTestObject(t *testing.T, repo *gogit.Repository, typ plumbing.ObjectType, encode func(plumbing.EncodedObject) error) plumbing.Hash {
	t.Helper()

	raw := repo.Storer.NewEncodedObject()
	raw.SetType(typ)

	if err := encode(raw); err != nil {
		t.Fatalf("encode %s object: %v", typ, err)
	}

	hash, err := repo.Storer.SetEncodedObject(raw)
	if err != nil {
		t.Fatalf("store %s object: %v", typ, err)
	}

	return hash
}

func writeTestBlob(t *testing.T, repo *gogit.Repository, content string) plumbing.Hash {
	t.Helper()

	return storeTestObject(t, repo, plumbing.BlobObject, func(raw plumbing.EncodedObject) error {
		w, err := raw.Writer()
		if err != nil {
			return err
		}

		if _, err := w.Write([]byte(content)); err != nil {
			return err
		}

		return w.Close()
	})
}

func writeTestTree(t *testing.T, repo *gogit.Repository, entries []object.TreeEntry) plumbing.Hash {
	t.Helper()

	tree := &object.Tree{Entries: entries}

	return storeTestObject(t, repo, plumbing.TreeObject, func(raw plumbing.EncodedObject) error {
		return tree.Encode(raw)
	})
}

// TestExportTree_SubmoduleBelowRepositoryRoot guards against .gitmodules
// being consulted per subtree: it only exists at the repository root and its
// paths are root-relative, so a submodule in a subdirectory would otherwise
// find no configuration and be exported as a silently empty directory.
func TestExportTree_SubmoduleBelowRepositoryRoot(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()

	subPath := filepath.Join(tmp, "sub")
	subRepo := initLocalTestRepo(t, subPath)
	commitLocalTestFile(t, subRepo, subPath, "marker.txt", "from submodule\n", "add marker")

	subHead, err := subRepo.Head()
	if err != nil {
		t.Fatalf("submodule Head() error = %v", err)
	}

	parentPath := filepath.Join(tmp, "parent")
	parentRepo := initLocalTestRepo(t, parentPath)

	// exportTree resolves submodules against the parent's primary remote.
	if _, err := parentRepo.CreateRemote(&config.RemoteConfig{
		Name: git.RemoteName,
		URLs: []string{"file://" + parentPath},
	}); err != nil {
		t.Fatalf("create parent remote: %v", err)
	}

	gitmodules := fmt.Sprintf("[submodule \"nested/sub\"]\n\tpath = nested/sub\n\turl = file://%s\n", subPath)

	// Build the parent commit by hand: go-git's worktree cannot stage a
	// gitlink entry.
	nestedTree := writeTestTree(t, parentRepo, []object.TreeEntry{
		{Name: "sub", Mode: filemode.Submodule, Hash: subHead.Hash()},
	})

	rootTree := writeTestTree(t, parentRepo, []object.TreeEntry{
		{Name: ".gitmodules", Mode: filemode.Regular, Hash: writeTestBlob(t, parentRepo, gitmodules)},
		{Name: "nested", Mode: filemode.Dir, Hash: nestedTree},
	})

	signature := object.Signature{Name: "submodule-test", Email: "submodule-test@example.com", When: time.Now()}
	commitHash := storeTestObject(t, parentRepo, plumbing.CommitObject, func(raw plumbing.EncodedObject) error {
		return (&object.Commit{
			Author:    signature,
			Committer: signature,
			Message:   "add nested submodule\n",
			TreeHash:  rootTree,
		}).Encode(raw)
	})

	exportDir := filepath.Join(tmp, "export")
	if err := git.ExportTree(exportDir, parentRepo, commitHash, git.ExportOptions{
		SubmoduleCacheDir: filepath.Join(tmp, "submodules"),
	}); err != nil {
		t.Fatalf("ExportTree() error = %v", err)
	}

	marker, err := os.ReadFile(filepath.Join(exportDir, "nested", "sub", "marker.txt"))
	if err != nil {
		t.Fatalf("read file from submodule below repository root: %v", err)
	}

	if string(marker) != "from submodule\n" {
		t.Fatalf("submodule marker.txt = %q, want %q", marker, "from submodule\n")
	}
}

// TestExportTree_SubmoduleReferencingItself exports a submodule whose tree
// pins an older commit of itself. Its mirror is read under a shared lock while
// exporting, so the nested export must wait for that lock's release before
// fetching into the same mirror, or it deadlocks.
func TestExportTree_SubmoduleReferencingItself(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()

	subPath := filepath.Join(tmp, "sub")
	subRepo := initLocalTestRepo(t, subPath)
	older := commitLocalTestFile(t, subRepo, subPath, "marker.txt", "older\n", "add marker")

	signature := object.Signature{Name: "submodule-test", Email: "submodule-test@example.com", When: time.Now()}
	subGitmodules := fmt.Sprintf("[submodule \"self\"]\n\tpath = self\n\turl = file://%s\n", subPath)

	newer := storeTestObject(t, subRepo, plumbing.CommitObject, func(raw plumbing.EncodedObject) error {
		return (&object.Commit{
			Author:    signature,
			Committer: signature,
			Message:   "pin older self\n",
			TreeHash: writeTestTree(t, subRepo, []object.TreeEntry{
				{Name: ".gitmodules", Mode: filemode.Regular, Hash: writeTestBlob(t, subRepo, subGitmodules)},
				{Name: "marker.txt", Mode: filemode.Regular, Hash: writeTestBlob(t, subRepo, "newer\n")},
				{Name: "self", Mode: filemode.Submodule, Hash: older},
			}),
			ParentHashes: []plumbing.Hash{older},
		}).Encode(raw)
	})

	if err := subRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), newer)); err != nil {
		t.Fatalf("advance submodule main: %v", err)
	}

	parentPath := filepath.Join(tmp, "parent")
	parentRepo := initLocalTestRepo(t, parentPath)

	if _, err := parentRepo.CreateRemote(&config.RemoteConfig{
		Name: git.RemoteName,
		URLs: []string{"file://" + parentPath},
	}); err != nil {
		t.Fatalf("create parent remote: %v", err)
	}

	parentGitmodules := fmt.Sprintf("[submodule \"sub\"]\n\tpath = sub\n\turl = file://%s\n", subPath)

	commitHash := storeTestObject(t, parentRepo, plumbing.CommitObject, func(raw plumbing.EncodedObject) error {
		return (&object.Commit{
			Author:    signature,
			Committer: signature,
			Message:   "add submodule\n",
			TreeHash: writeTestTree(t, parentRepo, []object.TreeEntry{
				{Name: ".gitmodules", Mode: filemode.Regular, Hash: writeTestBlob(t, parentRepo, parentGitmodules)},
				{Name: "sub", Mode: filemode.Submodule, Hash: newer},
			}),
		}).Encode(raw)
	})

	exportDir := filepath.Join(tmp, "export")
	done := make(chan error, 1)

	go func() {
		done <- git.ExportTree(exportDir, parentRepo, commitHash, git.ExportOptions{
			SubmoduleCacheDir: filepath.Join(tmp, "submodules"),
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ExportTree() error = %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("ExportTree() deadlocked on a self-referencing submodule")
	}

	for rel, want := range map[string]string{
		filepath.Join("sub", "marker.txt"):         "newer\n",
		filepath.Join("sub", "self", "marker.txt"): "older\n",
	} {
		got, err := os.ReadFile(filepath.Join(exportDir, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}

		if string(got) != want {
			t.Fatalf("%s = %q, want %q", rel, got, want)
		}
	}
}

// TestExportTree_ReusesCachedSubmoduleCommit exports submodule commits from
// their cache mirror without fetching once the mirror holds them, and still
// fetches a commit the mirror does not hold yet.
func TestExportTree_ReusesCachedSubmoduleCommit(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()

	subPath := filepath.Join(tmp, "sub")
	subRepo := initLocalTestRepo(t, subPath)
	first := commitLocalTestFile(t, subRepo, subPath, "marker.txt", "first\n", "add marker")

	parentPath := filepath.Join(tmp, "parent")
	parentRepo := initLocalTestRepo(t, parentPath)

	if _, err := parentRepo.CreateRemote(&config.RemoteConfig{
		Name: git.RemoteName,
		URLs: []string{"file://" + parentPath},
	}); err != nil {
		t.Fatalf("create parent remote: %v", err)
	}

	gitmodules := writeTestBlob(t, parentRepo, fmt.Sprintf("[submodule \"sub\"]\n\tpath = sub\n\turl = file://%s\n", subPath))
	signature := object.Signature{Name: "submodule-test", Email: "submodule-test@example.com", When: time.Now()}

	parentCommit := func(subCommit plumbing.Hash, readme string) plumbing.Hash {
		return storeTestObject(t, parentRepo, plumbing.CommitObject, func(raw plumbing.EncodedObject) error {
			return (&object.Commit{
				Author:    signature,
				Committer: signature,
				Message:   "pin submodule\n",
				TreeHash: writeTestTree(t, parentRepo, []object.TreeEntry{
					{Name: ".gitmodules", Mode: filemode.Regular, Hash: gitmodules},
					{Name: "README.md", Mode: filemode.Regular, Hash: writeTestBlob(t, parentRepo, readme)},
					{Name: "sub", Mode: filemode.Submodule, Hash: subCommit},
				}),
			}).Encode(raw)
		})
	}

	opts := git.ExportOptions{SubmoduleCacheDir: filepath.Join(tmp, "submodules")}

	export := func(name string, commit plumbing.Hash, want string) {
		t.Helper()

		exportDir := filepath.Join(tmp, name)
		if err := git.ExportTree(exportDir, parentRepo, commit, opts); err != nil {
			t.Fatalf("ExportTree(%s) error = %v", name, err)
		}

		got, err := os.ReadFile(filepath.Join(exportDir, "sub", "marker.txt"))
		if err != nil {
			t.Fatalf("read %s submodule marker: %v", name, err)
		}

		if string(got) != want {
			t.Fatalf("%s submodule marker.txt = %q, want %q", name, got, want)
		}
	}

	export("initial", parentCommit(first, "v1\n"), "first\n")

	// A fetch from the moved remote fails, so this export only succeeds if it
	// reads the cached commit without fetching.
	movedPath := subPath + "-moved"
	if err := os.Rename(subPath, movedPath); err != nil {
		t.Fatalf("move submodule remote: %v", err)
	}

	export("cached", parentCommit(first, "v2\n"), "first\n")

	if err := os.Rename(movedPath, subPath); err != nil {
		t.Fatalf("restore submodule remote: %v", err)
	}

	second := commitLocalTestFile(t, subRepo, subPath, "marker.txt", "second\n", "update marker")

	export("fetched", parentCommit(second, "v3\n"), "second\n")
}

// TestExportTree_ReportsSubmoduleMirrorPacks reports a submodule mirror's
// packfiles when it is cloned and when its fetch is skipped, since a mirror
// that is never fetched again would otherwise not be reported after a restart.
//
// Not parallel: the pack observer is process-global.
func TestExportTree_ReportsSubmoduleMirrorPacks(t *testing.T) {
	var (
		mu      sync.Mutex
		reports []git.MirrorPackStats
	)

	git.SetMirrorPackObserver(func(s git.MirrorPackStats) {
		mu.Lock()
		defer mu.Unlock()

		reports = append(reports, s)
	})
	t.Cleanup(func() { git.SetMirrorPackObserver(nil) })

	tmp := t.TempDir()

	subPath := filepath.Join(tmp, "sub")
	subRepo := initLocalTestRepo(t, subPath)
	subCommit := commitLocalTestFile(t, subRepo, subPath, "marker.txt", "marker\n", "add marker")
	subURL := "file://" + subPath

	parentPath := filepath.Join(tmp, "parent")
	parentRepo := initLocalTestRepo(t, parentPath)

	if _, err := parentRepo.CreateRemote(&config.RemoteConfig{
		Name: git.RemoteName,
		URLs: []string{"file://" + parentPath},
	}); err != nil {
		t.Fatalf("create parent remote: %v", err)
	}

	signature := object.Signature{Name: "submodule-test", Email: "submodule-test@example.com", When: time.Now()}
	parentCommit := storeTestObject(t, parentRepo, plumbing.CommitObject, func(raw plumbing.EncodedObject) error {
		return (&object.Commit{
			Author:    signature,
			Committer: signature,
			Message:   "add submodule\n",
			TreeHash: writeTestTree(t, parentRepo, []object.TreeEntry{
				{Name: ".gitmodules", Mode: filemode.Regular, Hash: writeTestBlob(t, parentRepo, fmt.Sprintf("[submodule \"sub\"]\n\tpath = sub\n\turl = %s\n", subURL))},
				{Name: "sub", Mode: filemode.Submodule, Hash: subCommit},
			}),
		}).Encode(raw)
	})

	opts := git.ExportOptions{SubmoduleCacheDir: filepath.Join(tmp, "submodules")}

	for _, name := range []string{"cloned", "cached"} {
		mu.Lock()
		reports = nil
		mu.Unlock()

		if err := git.ExportTree(filepath.Join(tmp, name), parentRepo, parentCommit, opts); err != nil {
			t.Fatalf("ExportTree(%s) error = %v", name, err)
		}

		mu.Lock()
		got := reports
		mu.Unlock()

		if len(got) != 1 {
			t.Fatalf("%s: observer received %d reports, want 1: %+v", name, len(got), got)
		}

		if got[0].Repository != git.GetRepoName(subURL) || filepath.Dir(got[0].Path) != opts.SubmoduleCacheDir ||
			got[0].PacksAfter != 1 || got[0].SizeBytes <= 0 || got[0].Result != "" {
			t.Fatalf("%s: observer stats = %+v, want one pack of the submodule mirror", name, got[0])
		}
	}
}
