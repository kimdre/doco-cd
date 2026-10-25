package git_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gittransport "github.com/go-git/go-git/v5/plumbing/transport/git"

	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/test"
)

func TestNativeGitBareMirrorCloneAndFetch(t *testing.T) {
	source, sourcePath, sourceURL := nativeTestRepository(t)

	initial, err := source.Head()
	if err != nil {
		t.Fatalf("get source HEAD: %v", err)
	}

	mirror, mirrorPath := cloneNativeTestMirror(t, sourceURL, git.MainBranch, 0)
	assertNativeMirrorCommit(t, mirror, git.MainBranch, initial.Hash())
	assertNativeMirrorBare(t, mirror, mirrorPath)

	updated := commitLocalTestFile(t, source, sourcePath, "README.md", "updated over native Git\n", "update README")

	mirror, err = git.CloneOrUpdateBareMirror(nil, sourceURL, git.MainBranch, mirrorPath,
		false, "", "", false, transport.ProxyOptions{}, 0)
	if err != nil {
		t.Fatalf("fetch native Git mirror: %v", err)
	}

	assertNativeMirrorCommit(t, mirror, git.MainBranch, updated)
	assertNativeMirrorBare(t, mirror, mirrorPath)

	commit, err := mirror.CommitObject(updated)
	if err != nil {
		t.Fatalf("read fetched commit: %v", err)
	}

	file, err := commit.File("README.md")
	if err != nil {
		t.Fatalf("read fetched README: %v", err)
	}

	content, err := file.Contents()
	if err != nil || content != "updated over native Git\n" {
		t.Fatalf("fetched README = %q, error = %v", content, err)
	}
}

func TestNativeGitBareMirrorShallowDepth(t *testing.T) {
	source, sourcePath, sourceURL := nativeTestRepository(t)

	initial, err := source.Head()
	if err != nil {
		t.Fatalf("get initial source HEAD: %v", err)
	}

	boundary := commitLocalTestFile(t, source, sourcePath, "README.md", "second\n", "second commit")
	tip := commitLocalTestFile(t, source, sourcePath, "README.md", "third\n", "third commit")
	mirror, _ := cloneNativeTestMirror(t, sourceURL, git.MainBranch, 2)
	assertNativeMirrorCommit(t, mirror, git.MainBranch, tip)

	shallow, err := mirror.Storer.Shallow()
	if err != nil {
		t.Fatalf("read shallow boundaries: %v", err)
	}

	if len(shallow) != 1 || shallow[0] != boundary {
		t.Fatalf("shallow boundaries = %v, want [%s]", shallow, boundary)
	}

	for _, hash := range []plumbing.Hash{tip, boundary} {
		if _, err := mirror.CommitObject(hash); err != nil {
			t.Fatalf("depth-2 commit %s missing: %v", hash, err)
		}
	}

	if _, err := mirror.CommitObject(initial.Hash()); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("commit outside shallow depth error = %v, want %v", err, plumbing.ErrObjectNotFound)
	}

	commits, err := mirror.Storer.IterEncodedObjects(plumbing.CommitObject)
	if err != nil {
		t.Fatalf("iterate shallow commit objects: %v", err)
	}

	count := 0

	if err := commits.ForEach(func(plumbing.EncodedObject) error {
		count++

		return nil
	}); err != nil {
		t.Fatalf("count shallow commit objects: %v", err)
	}

	if count != 2 {
		t.Fatalf("depth-2 mirror has %d commit objects, want 2", count)
	}
}

func TestNativeGitExportBranchTagAndPinnedCommit(t *testing.T) {
	source, sourcePath, sourceURL := nativeTestRepository(t)
	pinned := commitLocalTestFile(t, source, sourcePath, "version.txt", "pinned\n", "pinned version")
	tagged := commitLocalTestFile(t, source, sourcePath, "version.txt", "tagged\n", "tagged version")

	if _, err := source.CreateTag("v1.0.0", tagged, &gogit.CreateTagOptions{
		Tagger:  &object.Signature{Name: "native-test", Email: "native-test@example.invalid", When: time.Now()},
		Message: "native Git annotated tag",
	}); err != nil {
		t.Fatalf("create annotated tag: %v", err)
	}

	branch := commitLocalTestFile(t, source, sourcePath, "version.txt", "branch\n", "branch version")
	if err := source.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("release"), branch)); err != nil {
		t.Fatalf("create release branch: %v", err)
	}

	main := commitLocalTestFile(t, source, sourcePath, "version.txt", "main\n", "main version")

	for _, tc := range []struct {
		name    string
		ref     string
		hash    plumbing.Hash
		content string
	}{
		{name: "main", ref: git.MainBranch, hash: main, content: "main\n"},
		{name: "branch", ref: "refs/heads/release", hash: branch, content: "branch\n"},
		{name: "tag", ref: "refs/tags/v1.0.0", hash: tagged, content: "tagged\n"},
		{name: "pinned commit", ref: pinned.String(), hash: pinned, content: "pinned\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mirror, _ := cloneNativeTestMirror(t, sourceURL, tc.ref, 0)
			assertNativeMirrorCommit(t, mirror, tc.ref, tc.hash)

			exportPath := filepath.Join(t.TempDir(), "export")
			if err := git.ExportTree(exportPath, mirror, tc.hash, git.ExportOptions{}); err != nil {
				t.Fatalf("export native Git tree: %v", err)
			}

			content, err := os.ReadFile(filepath.Join(exportPath, "version.txt"))
			if err != nil || string(content) != tc.content {
				t.Fatalf("exported version = %q, error = %v, want %q", content, err, tc.content)
			}

			if _, err := os.Stat(filepath.Join(exportPath, ".git")); !os.IsNotExist(err) {
				t.Fatalf("export must not contain Git metadata: %v", err)
			}
		})
	}
}

func TestNativeGitUppercaseSchemeIgnoresHTTPToken(t *testing.T) {
	source, _, sourceURL := nativeTestRepository(t)

	head, err := source.Head()
	if err != nil {
		t.Fatalf("get source HEAD: %v", err)
	}

	// go-git treats the scheme case-insensitively, so credential selection must do the same.
	mirror, _ := cloneNativeTestMirror(t, "GIT"+strings.TrimPrefix(sourceURL, "git"), git.MainBranch, 0)
	assertNativeMirrorCommit(t, mirror, git.MainBranch, head.Hash())
}

func TestNativeGitPrivateSourceRejectsHTTPToken(t *testing.T) {
	t.Parallel()

	// The private check runs before any network access, so this test needs no daemon.
	sourceURL := "git://127.0.0.1:1/repo.git"
	mirrorPath := filepath.Join(t.TempDir(), "mirror")

	_, err := git.CloneOrUpdateBareMirror(nil, sourceURL, git.MainBranch, mirrorPath,
		true, "", "", false, transport.ProxyOptions{}, 0)
	if !errors.Is(err, git.ErrMissingAuthToken) {
		t.Fatalf("private native Git source error = %v, want %v", err, git.ErrMissingAuthToken)
	}

	if _, err := os.Stat(mirrorPath); !os.IsNotExist(err) {
		t.Fatalf("rejected private native source created a mirror: %v", err)
	}
}

func TestNativeGitDefaultEndpointPort(t *testing.T) {
	if gittransport.DefaultPort != 9418 {
		t.Fatalf("native Git transport default port = %d, want 9418", gittransport.DefaultPort)
	}

	for _, tc := range []struct {
		name string
		url  string
		port int
	}{
		{name: "implicit", url: "git://127.0.0.1/repo.git", port: 0},
		{name: "explicit", url: "git://127.0.0.1:9418/repo.git", port: 9418},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, err := transport.NewEndpoint(tc.url)
			if err != nil {
				t.Fatalf("parse native Git default-port endpoint: %v", err)
			}

			// An unspecified port stays zero until the native transport supplies 9418.
			if endpoint.Protocol != "git" || endpoint.Host != "127.0.0.1" || endpoint.Port != tc.port || endpoint.Path != "/repo.git" {
				t.Fatalf("native Git endpoint = %#v, want git protocol, loopback host, port %d, and /repo.git path", endpoint, tc.port)
			}
		})
	}
}

func TestNativeGitRelativeSubmodulePinnedCommit(t *testing.T) {
	root := t.TempDir()
	siblingPath := filepath.Join(root, "sibling.git")
	sibling := initLocalTestRepo(t, siblingPath)
	pinned := commitLocalTestFile(t, sibling, siblingPath, "marker.txt", "pinned sibling\n", "pinned sibling")
	newer := commitLocalTestFile(t, sibling, siblingPath, "marker.txt", "newer sibling\n", "advance sibling")
	parentPath := filepath.Join(root, "parent.git")
	parent := initLocalTestRepo(t, parentPath)

	parentHead, err := parent.Head()
	if err != nil {
		t.Fatalf("get parent HEAD: %v", err)
	}

	gitmodules := "[submodule \"sibling\"]\n\tpath = sibling\n\turl = ../sibling.git\n"
	tree := writeTestTree(t, parent, []object.TreeEntry{
		{Name: ".gitmodules", Mode: filemode.Regular, Hash: writeTestBlob(t, parent, gitmodules)},
		{Name: "sibling", Mode: filemode.Submodule, Hash: pinned},
	})
	signature := object.Signature{Name: "native-submodule-test", Email: "native-submodule-test@example.invalid", When: time.Now()}

	parentCommit := storeTestObject(t, parent, plumbing.CommitObject, func(raw plumbing.EncodedObject) error {
		return (&object.Commit{
			Author:       signature,
			Committer:    signature,
			Message:      "pin native relative submodule\n",
			TreeHash:     tree,
			ParentHashes: []plumbing.Hash{parentHead.Hash()},
		}).Encode(raw)
	})
	if err := parent.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), parentCommit)); err != nil {
		t.Fatalf("advance parent main: %v", err)
	}

	baseURL := test.StartGitDaemon(t, root)
	mirror, _ := cloneNativeTestMirror(t, baseURL+"parent.git", git.MainBranch, 0)
	assertNativeMirrorCommit(t, mirror, git.MainBranch, parentCommit)

	commit, err := mirror.CommitObject(parentCommit)
	if err != nil {
		t.Fatalf("read parent mirror commit: %v", err)
	}

	parentTree, err := commit.Tree()
	if err != nil {
		t.Fatalf("read parent mirror tree: %v", err)
	}

	gitlink, err := parentTree.FindEntry("sibling")
	if err != nil || gitlink.Mode != filemode.Submodule || gitlink.Hash != pinned || gitlink.Hash == newer {
		t.Fatalf("native parent gitlink = %+v, error = %v, want pinned submodule %s", gitlink, err, pinned)
	}

	exportPath := filepath.Join(t.TempDir(), "export")
	if err := git.ExportTree(exportPath, mirror, parentCommit, git.ExportOptions{
		SubmoduleCacheDir: filepath.Join(t.TempDir(), "submodules"),
	}); err != nil {
		t.Fatalf("export native relative submodule: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(exportPath, "sibling", "marker.txt"))
	if err != nil || string(content) != "pinned sibling\n" {
		t.Fatalf("native relative submodule marker = %q, error = %v, want pinned content", content, err)
	}
}

func nativeTestRepository(t *testing.T) (*gogit.Repository, string, string) {
	t.Helper()

	root := t.TempDir()
	path := filepath.Join(root, "repo.git")
	repo := initLocalTestRepo(t, path)
	baseURL := test.StartGitDaemon(t, root)

	return repo, path, baseURL + "repo.git"
}

func cloneNativeTestMirror(t *testing.T, sourceURL, ref string, depth int) (*gogit.Repository, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "mirror")

	repo, err := git.CloneOrUpdateBareMirror(nil, sourceURL, ref, path,
		false, "", "", false, transport.ProxyOptions{}, depth)
	if err != nil {
		t.Fatalf("clone native Git mirror at %s: %v", ref, err)
	}

	return repo, path
}

func assertNativeMirrorCommit(t *testing.T, repo *gogit.Repository, ref string, want plumbing.Hash) {
	t.Helper()

	got, err := git.GetLatestCommit(repo, ref)
	if err != nil || got != want.String() {
		t.Fatalf("native mirror commit for %s = %s, error = %v, want %s", ref, got, err, want)
	}
}

func assertNativeMirrorBare(t *testing.T, repo *gogit.Repository, path string) {
	t.Helper()

	if _, err := repo.Worktree(); !errors.Is(err, gogit.ErrIsBareRepository) {
		t.Fatalf("native mirror Worktree() error = %v, want %v", err, gogit.ErrIsBareRepository)
	}

	if _, err := os.Stat(filepath.Join(path, ".git")); !os.IsNotExist(err) {
		t.Fatalf("native mirror must not contain a .git directory: %v", err)
	}
}
