package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// initLocalTestRepo creates a local (non-bare) git repository at path with
// an initial commit on "main", returning the go-git handle.
func initLocalTestRepo(t *testing.T, path string) *gogit.Repository {
	t.Helper()

	repo, err := gogit.PlainInit(path, false)
	if err != nil {
		t.Fatalf("failed to init local test repo: %v", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("failed to set HEAD to main: %v", err)
	}

	commitTestFile(t, repo, path, "README.md", "initial\n", "initial commit")

	return repo
}

func commitTestFile(t *testing.T, repo *gogit.Repository, repoPath, relPath, content, msg string) plumbing.Hash {
	t.Helper()

	filePath := filepath.Join(repoPath, relPath)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("failed to create parent directory for %s: %v", relPath, err)
	}

	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write %s: %v", relPath, err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := wt.Add(relPath); err != nil {
		t.Fatalf("failed to add %s: %v", relPath, err)
	}

	hash, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: "store-test", Email: "store-test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit %q: %v", msg, err)
	}

	return hash
}

func newGitStore(t *testing.T, cloneURL string) *store.GitStore {
	t.Helper()

	s, err := store.NewGitStore(store.GitStoreOptions{
		CloneURL:     cloneURL,
		BaseDir:      t.TempDir(),
		ProxyOptions: transport.ProxyOptions{},
	})
	if err != nil {
		t.Fatalf("NewGitStore() error = %v", err)
	}

	return s
}

func TestNewGitStore_RequiresCloneURLAndBaseDir(t *testing.T) {
	t.Parallel()

	if _, err := store.NewGitStore(store.GitStoreOptions{BaseDir: t.TempDir()}); err == nil {
		t.Fatal("expected an error for missing CloneURL")
	}

	if _, err := store.NewGitStore(store.GitStoreOptions{CloneURL: "file:///tmp/repo"}); err == nil {
		t.Fatal("expected an error for missing BaseDir")
	}
}

func TestGitStore_ResolveThenPublish(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	s := newGitStore(t, "file://"+srcPath)

	revision, err := s.Resolve(t.Context(), git.MainBranch)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if revision == "" {
		t.Fatal("Resolve() returned an empty revision")
	}

	artifact, err := s.Publish(t.Context(), revision)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	if artifact.Revision != revision {
		t.Fatalf("Publish() revision = %q, want %q", artifact.Revision, revision)
	}

	readme, err := os.ReadFile(filepath.Join(artifact.Path, "README.md"))
	if err != nil {
		t.Fatalf("read exported README.md: %v", err)
	}

	if string(readme) != "initial\n" {
		t.Fatalf("README.md content = %q, want %q", readme, "initial\n")
	}
}

func TestGitStore_PublishUnknownRevision_ReturnsErrRevisionNotFound(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	s := newGitStore(t, "file://"+srcPath)

	// Resolve first so the mirror exists locally, but never fetch the
	// unknown revision below.
	if _, err := s.Resolve(t.Context(), git.MainBranch); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	unknown := store.Revision("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	_, err := s.Publish(t.Context(), unknown)
	if !errors.Is(err, store.ErrRevisionNotFound) {
		t.Fatalf("Publish() error = %v, want ErrRevisionNotFound", err)
	}
}

// TestGitStore_ResolvePinnedUnreachableCommit covers a compose include pinned
// to a full SHA whose branch was rebased afterwards: the commit still exists
// on the remote, but no branch or tag reaches it any more.
func TestGitStore_ResolvePinnedUnreachableCommit(t *testing.T) {
	t.Parallel()

	for _, warmMirror := range []bool{false, true} {
		name := "fresh mirror"
		if warmMirror {
			name = "existing mirror"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srcPath := filepath.Join(t.TempDir(), "src")
			repo := initLocalTestRepo(t, srcPath)

			head, err := repo.Head()
			if err != nil {
				t.Fatalf("Head() error = %v", err)
			}

			pinned := commitTestFile(t, repo, srcPath, "README.md", "pinned\n", "pinned commit")

			// Move main back, so the pinned commit is unreachable from any ref.
			if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), head.Hash())); err != nil {
				t.Fatalf("failed to reset main: %v", err)
			}

			s := newGitStore(t, "file://"+srcPath)

			if warmMirror {
				if _, err := s.Resolve(t.Context(), git.MainBranch); err != nil {
					t.Fatalf("Resolve(main) error = %v", err)
				}
			}

			revision, err := s.Resolve(t.Context(), pinned.String())
			if err != nil {
				t.Fatalf("Resolve(%s) error = %v", pinned, err)
			}

			if revision != store.Revision(pinned.String()) {
				t.Fatalf("Resolve() revision = %q, want %q", revision, pinned)
			}

			artifact, err := s.Publish(t.Context(), revision)
			if err != nil {
				t.Fatalf("Publish() error = %v", err)
			}

			readme, err := os.ReadFile(filepath.Join(artifact.Path, "README.md"))
			if err != nil {
				t.Fatalf("read exported README.md: %v", err)
			}

			if string(readme) != "pinned\n" {
				t.Fatalf("README.md content = %q, want %q", readme, "pinned\n")
			}
		})
	}
}

// TestGitStore_ResolveMissingCommit_Fails makes sure a SHA the remote does not
// have fails in Resolve, instead of passing through to Publish.
func TestGitStore_ResolveMissingCommit_Fails(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	s := newGitStore(t, "file://"+srcPath)

	if _, err := s.Resolve(t.Context(), "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); err == nil {
		t.Fatal("Resolve() error = nil, want an error for a commit the remote does not have")
	}
}

func TestGitStore_PublishIsIdempotent(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	s := newGitStore(t, "file://"+srcPath)

	revision, err := s.Resolve(t.Context(), git.MainBranch)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	first, err := s.Publish(t.Context(), revision)
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}

	second, err := s.Publish(t.Context(), revision)
	if err != nil {
		t.Fatalf("second Publish() error = %v", err)
	}

	if first.Path != second.Path {
		t.Fatalf("Publish() paths differ across calls: %q vs %q", first.Path, second.Path)
	}
}

func TestGitStore_PublishConcurrent_SameRevision(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	s := newGitStore(t, "file://"+srcPath)

	revision, err := s.Resolve(t.Context(), git.MainBranch)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	const workers = 8

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []store.Artifact
		errs    []error
	)

	for range workers {
		wg.Go(func() {
			artifact, err := s.Publish(t.Context(), revision)

			mu.Lock()
			defer mu.Unlock()

			results = append(results, artifact)
			errs = append(errs, err)
		})
	}

	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Publish() error = %v", err)
		}
	}

	for _, a := range results[1:] {
		if a.Path != results[0].Path {
			t.Fatalf("concurrent Publish() paths differ: %q vs %q", a.Path, results[0].Path)
		}
	}
}

// TestGitStore_PublishReusesArtifactPublishedWhileWaiting publishes a revision
// whose artifact appears while Publish waits for the mirror lock, as it does
// when another job publishes the same revision first. The mirror does not hold
// the revision, so Publish only succeeds if it looks the artifact up again
// instead of exporting the tree.
func TestGitStore_PublishReusesArtifactPublishedWhileWaiting(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	baseDir := t.TempDir()

	s, err := store.NewGitStore(store.GitStoreOptions{CloneURL: "file://" + srcPath, BaseDir: baseDir})
	if err != nil {
		t.Fatalf("NewGitStore() error = %v", err)
	}

	if _, err := s.Resolve(t.Context(), git.MainBranch); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	revision := store.Revision(strings.Repeat("ab", 20))

	unlock := git.AcquireExclusiveMirrorLock(s.MirrorDir())

	type result struct {
		artifact store.Artifact
		err      error
	}

	done := make(chan result, 1)

	go func() {
		artifact, err := s.Publish(t.Context(), revision)
		done <- result{artifact: artifact, err: err}
	}()

	// Give Publish time to miss the artifact and block on the lock. Should it
	// not get that far, it finds the artifact before locking and the test
	// passes without exercising the second lookup, but it never fails spuriously.
	time.Sleep(100 * time.Millisecond)

	artifactDir := filepath.Join(baseDir, store.ArtifactsSubdir, store.ArtifactDirName(revision))
	if err := os.MkdirAll(artifactDir, filesystem.PermDir); err != nil {
		unlock()
		t.Fatalf("create artifact: %v", err)
	}

	unlock()

	res := <-done
	if res.err != nil {
		t.Fatalf("Publish() error = %v", res.err)
	}

	if res.artifact.Path != artifactDir {
		t.Fatalf("Publish() path = %q, want %q", res.artifact.Path, artifactDir)
	}
}

func TestGitStore_PublishConcurrent_DifferentRevisions(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	repo := initLocalTestRepo(t, srcPath)

	s := newGitStore(t, "file://"+srcPath)

	first, err := s.Resolve(t.Context(), git.MainBranch)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	commitTestFile(t, repo, srcPath, "second.txt", "second\n", "second commit")

	second, err := s.Resolve(t.Context(), git.MainBranch)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if first == second {
		t.Fatalf("expected two distinct revisions, got %q for both", first)
	}

	revisions := []store.Revision{first, second}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []store.Artifact
		errs    []error
	)

	for _, revision := range revisions {
		wg.Add(1)

		go func(revision store.Revision) {
			defer wg.Done()

			artifact, err := s.Publish(t.Context(), revision)

			mu.Lock()
			defer mu.Unlock()

			results = append(results, artifact)
			errs = append(errs, err)
		}(revision)
	}

	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Publish() error = %v", err)
		}
	}

	firstArtifact, ok, err := s.Lookup(first)
	if err != nil || !ok {
		t.Fatalf("Lookup(first) = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if _, err := os.Stat(filepath.Join(firstArtifact.Path, "second.txt")); !os.IsNotExist(err) {
		t.Errorf("first revision's artifact must not contain second.txt (revisions must not interfere with each other), stat err = %v", err)
	}

	secondArtifact, ok, err := s.Lookup(second)
	if err != nil || !ok {
		t.Fatalf("Lookup(second) = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if _, err := os.Stat(filepath.Join(secondArtifact.Path, "second.txt")); err != nil {
		t.Errorf("second revision's artifact must contain second.txt: %v", err)
	}
}

func TestGitStore_LookupAndList(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	s := newGitStore(t, "file://"+srcPath)

	revision, err := s.Resolve(t.Context(), git.MainBranch)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if _, ok, err := s.Lookup(revision); err != nil || ok {
		t.Fatalf("Lookup() before Publish = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	published, err := s.Publish(t.Context(), revision)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	looked, ok, err := s.Lookup(revision)
	if err != nil || !ok {
		t.Fatalf("Lookup() after Publish = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if looked.Path != published.Path {
		t.Fatalf("Lookup() path = %q, want %q", looked.Path, published.Path)
	}

	all, err := s.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(all) != 1 || all[0].Revision != revision {
		t.Fatalf("List() = %+v, want a single entry for %q", all, revision)
	}
}

func TestGitStore_ResolveDifferentCommits(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	repo := initLocalTestRepo(t, srcPath)

	s := newGitStore(t, "file://"+srcPath)

	first, err := s.Resolve(t.Context(), git.MainBranch)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	commitTestFile(t, repo, srcPath, "CHANGED.md", "changed\n", "second commit")

	second, err := s.Resolve(t.Context(), git.MainBranch)
	if err != nil {
		t.Fatalf("second Resolve() error = %v", err)
	}

	if first == second {
		t.Fatal("Resolve() returned the same revision after a new commit")
	}

	artifact, err := s.Publish(t.Context(), second)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(artifact.Path, "CHANGED.md")); err != nil {
		t.Fatalf("expected CHANGED.md in artifact for %q: %v", second, err)
	}

	// The first revision's artifact must be unaffected by the new commit.
	firstArtifact, err := s.Publish(t.Context(), first)
	if err != nil {
		t.Fatalf("Publish(first) error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(firstArtifact.Path, "CHANGED.md")); !os.IsNotExist(err) {
		t.Fatalf("expected CHANGED.md to be absent from the first revision's artifact, stat err = %v", err)
	}
}

func TestGitStore_MirrorIsBare(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	baseDir := t.TempDir()

	s, err := store.NewGitStore(store.GitStoreOptions{
		CloneURL: "file://" + srcPath,
		BaseDir:  baseDir,
	})
	if err != nil {
		t.Fatalf("NewGitStore() error = %v", err)
	}

	if _, err := s.Resolve(t.Context(), git.MainBranch); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	mirrorDir := filepath.Join(baseDir, "mirror")

	repo, err := gogit.PlainOpen(mirrorDir)
	if err != nil {
		t.Fatalf("PlainOpen(mirror) error = %v", err)
	}

	if _, err := repo.Worktree(); !errors.Is(err, gogit.ErrIsBareRepository) {
		t.Fatalf("mirror Worktree() error = %v, want %v", err, gogit.ErrIsBareRepository)
	}

	if _, err := os.Stat(filepath.Join(mirrorDir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("expected no .git subdirectory in bare mirror, stat err = %v", err)
	}
}

func TestGitStore_ResolveHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	s := newGitStore(t, "file://"+srcPath)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := s.Resolve(ctx, git.MainBranch); !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve() error = %v, want context.Canceled", err)
	}
}

// An application repo routinely carries a symlink with an absolute target (a Laravel
// public/storage link into /var/www/html) far away from the directory an include needs.
// Git includes opt into skipping that link; regular Git stores reject it.
func TestGitStore_PublishAbsoluteSymlinkOptIn(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	repo := initLocalTestRepo(t, srcPath)

	commitTestFile(t, repo, srcPath, "deploy/compose.yaml", "services: {}\n", "add compose")

	if err := os.Symlink("/var/www/html/storage/app/public/src", filepath.Join(srcPath, "public-src")); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := wt.Add("public-src"); err != nil {
		t.Fatalf("failed to add symlink: %v", err)
	}

	pinned, err := wt.Commit("add absolute symlink", &gogit.CommitOptions{
		Author: &object.Signature{Name: "store-test", Email: "store-test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to commit symlink: %v", err)
	}

	strictStore := newGitStore(t, "file://"+srcPath)

	revision, err := strictStore.Resolve(t.Context(), pinned.String())
	if err != nil {
		t.Fatalf("Resolve(%s) with strict export error = %v", pinned, err)
	}

	if _, err := strictStore.Publish(t.Context(), revision); !errors.Is(err, filesystem.ErrPathTraversal) {
		t.Fatalf("Publish() error = %v, want path traversal error by default", err)
	}

	s, err := store.NewGitStore(store.GitStoreOptions{
		CloneURL:             "file://" + srcPath,
		BaseDir:              t.TempDir(),
		SkipAbsoluteSymlinks: true,
	})
	if err != nil {
		t.Fatalf("NewGitStore() error = %v", err)
	}

	revision, err = s.Resolve(t.Context(), pinned.String())
	if err != nil {
		t.Fatalf("Resolve(%s) error = %v", pinned, err)
	}

	artifact, err := s.Publish(t.Context(), revision)
	if err != nil {
		t.Fatalf("Publish() error = %v, want nil: an absolute symlink must be skipped, not fatal", err)
	}

	got, err := os.ReadFile(filepath.Join(artifact.Path, "deploy", "compose.yaml"))
	if err != nil {
		t.Fatalf("read deploy/compose.yaml from artifact: %v", err)
	}

	if string(got) != "services: {}\n" {
		t.Fatalf("deploy/compose.yaml = %q, want %q", got, "services: {}\n")
	}

	if _, statErr := os.Lstat(filepath.Join(artifact.Path, "public-src")); !os.IsNotExist(statErr) {
		t.Fatalf("expected public-src to be absent from the artifact, stat err = %v", statErr)
	}
}
