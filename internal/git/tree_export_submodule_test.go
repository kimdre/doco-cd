package git_test

import (
	"fmt"
	"os"
	"path/filepath"
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
