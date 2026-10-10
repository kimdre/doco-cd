package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage"
	gitfs "github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/filesystem/dotgit"
	"github.com/go-git/go-git/v5/storage/memory"
)

const namespaceDestination = "refs/remotes/origin/release"

func namespaceFixture(t *testing.T) (*git.Repository, billy.Filesystem, error) {
	t.Helper()

	repo, err := git.PlainInit(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}

	fs, err := referenceFilesystem(repo)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []plumbing.ReferenceName{
		namespaceDestination + "/old", "refs/remotes/origin/keep",
		"refs/heads/local", "refs/remotes/upstream/release", "refs/tags/unrelated",
	} {
		if err := repo.Storer.SetReference(plumbing.NewHashReference(name, plumbing.NewHash(strings.Repeat("1", 40)))); err != nil {
			t.Fatal(err)
		}
	}

	return repo, fs, fmt.Errorf("fetch: %w", &os.PathError{
		Op: "open", Path: filepath.Join(fs.Root(), namespaceDestination), Err: syscall.EISDIR,
	})
}

func namespaceAdvertisement() []*plumbing.Reference {
	return []*plumbing.Reference{
		plumbing.NewHashReference("refs/heads/release", plumbing.NewHash(strings.Repeat("2", 40))),
	}
}

func namespaceOptions() *git.FetchOptions {
	return &git.FetchOptions{RefSpecs: []config.RefSpec{"+refs/heads/release:refs/remotes/origin/release"}}
}

func TestRefNamespaceRecoveryBudget(t *testing.T) {
	t.Parallel()

	for _, retryErr := range []error{nil, os.ErrPermission, syscall.EISDIR, plumbing.ErrObjectNotFound} {
		t.Run(fmt.Sprint(retryErr), func(t *testing.T) {
			t.Parallel()

			repo, fs, original := namespaceFixture(t)
			fetches, listings := 0, 0
			err := fetchWithRefNamespaceRecovery(repo, namespaceOptions(), func() error {
				fetches++
				if fetches == 1 {
					return original
				}

				return retryErr
			}, func() ([]*plumbing.Reference, error) {
				listings++
				return namespaceAdvertisement(), nil
			})

			if fetches != 2 || listings != 1 {
				t.Fatalf("fetches/listings = %d/%d, want 2/1", fetches, listings)
			}

			if retryErr == nil {
				if err != nil {
					t.Fatal(err)
				}
			} else if !isRefNamespaceError(err) || !errors.Is(err, retryErr) || !errors.Is(err, original) || isFocusedFetchFallbackError(err) {
				t.Fatalf("retry failure lost causes or allows fallback: %v", err)
			}

			if _, err := fs.Lstat(namespaceDestination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("blocking directory remains: %v", err)
			}

			for _, name := range []plumbing.ReferenceName{
				"refs/remotes/origin/keep", "refs/heads/local", "refs/remotes/upstream/release", "refs/tags/unrelated",
			} {
				if _, err := repo.Reference(name, false); err != nil {
					t.Fatalf("unrelated reference %s removed: %v", name, err)
				}
			}

			for _, root := range []string{"refs", "refs/remotes", "refs/remotes/origin"} {
				if err := requireRefDirectory(fs, root); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRefNamespaceOrdinaryFetchDoesNotList(t *testing.T) {
	t.Parallel()

	repo, _, _ := namespaceFixture(t)
	for _, fetchErr := range []error{nil, os.ErrPermission, errors.New("is a directory")} {
		t.Run(fmt.Sprint(fetchErr), func(t *testing.T) {
			calls := 0
			err := fetchWithRefNamespaceRecovery(repo, namespaceOptions(), func() error {
				calls++
				return fetchErr
			}, func() ([]*plumbing.Reference, error) {
				t.Fatal("ordinary fetch listed the remote")
				return nil, nil
			})

			if !errors.Is(err, fetchErr) || calls != 1 {
				t.Fatalf("fetch result = %v, calls = %d", err, calls)
			}
		})
	}
}

func TestRefNamespaceRefusesUnsafeCleanup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(*testing.T, *git.Repository, billy.Filesystem)
		refs  []*plumbing.Reference
		err   error
	}{
		{name: "discovery failure", err: os.ErrPermission},
		{name: "missing destination", refs: []*plumbing.Reference{}},
		{name: "live blocker", refs: append(namespaceAdvertisement(),
			plumbing.NewHashReference("refs/heads/release/old", plumbing.NewHash(strings.Repeat("1", 40))))},
		{name: "live symbolic blocker", refs: append(namespaceAdvertisement(),
			plumbing.NewSymbolicReference("refs/heads/release/old", "refs/heads/release"))},
		{name: "advertised zero hash blocker", refs: append(namespaceAdvertisement(),
			plumbing.NewHashReference("refs/heads/release/old", plumbing.ZeroHash))},
		{name: "unknown file", setup: func(t *testing.T, _ *git.Repository, fs billy.Filesystem) {
			writeNamespaceFile(t, fs, namespaceDestination+"/unknown", "not a reference\n")
		}},
		{name: "symbolic blocker", setup: func(t *testing.T, repo *git.Repository, _ billy.Filesystem) {
			if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(
				namespaceDestination+"/symbolic", "refs/remotes/origin/keep")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "leaf symlink", setup: func(t *testing.T, _ *git.Repository, fs billy.Filesystem) {
			if err := fs.Symlink("../keep", namespaceDestination+"/link"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory symlink", setup: func(t *testing.T, _ *git.Repository, fs billy.Filesystem) {
			if err := fs.Symlink(t.TempDir(), namespaceDestination+"/link"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "invalid ref filename", setup: func(t *testing.T, _ *git.Repository, fs billy.Filesystem) {
			writeNamespaceFile(t, fs, namespaceDestination+"/unknown.lock", strings.Repeat("1", 40)+"\n")
		}},
		{name: "malformed packed refs", setup: func(t *testing.T, _ *git.Repository, fs billy.Filesystem) {
			writeNamespaceFile(t, fs, "packed-refs", "invalid reference\n")
		}},
		{name: "packed refs symlink", setup: func(t *testing.T, _ *git.Repository, fs billy.Filesystem) {
			if err := fs.Symlink(namespaceDestination+"/old", "packed-refs"); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo, fs, original := namespaceFixture(t)
			if tt.setup != nil {
				tt.setup(t, repo, fs)
			}

			fetches := 0
			err := fetchWithRefNamespaceRecovery(repo, namespaceOptions(), func() error {
				fetches++
				return original
			}, func() ([]*plumbing.Reference, error) {
				refs := tt.refs
				if refs == nil {
					refs = namespaceAdvertisement()
				}

				return refs, tt.err
			})

			if !isRefNamespaceError(err) || !errors.Is(err, original) || isFocusedFetchFallbackError(err) || fetches != 1 {
				t.Fatalf("unsafe recovery = %v, fetches = %d", err, fetches)
			}

			if tt.err != nil && !errors.Is(err, tt.err) {
				t.Fatalf("discovery error lost: %v", err)
			}

			if _, err := fs.Lstat(namespaceDestination + "/old"); err != nil {
				t.Fatalf("preflight deleted a stale ref before refusing cleanup: %v", err)
			}
		})
	}
}

func writeNamespaceFile(t *testing.T, fs billy.Filesystem, name, content string) {
	t.Helper()

	file, err := fs.Create(name)
	if err != nil {
		t.Fatal(err)
	}

	_, writeErr := file.Write([]byte(content))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestRefNamespaceValidatesErrorPath(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"HEAD", "objects/release", "refs/heads/release", "refs/remotes/upstream/release",
		"refs/remotes/origin", "refs/remotes/origin/other", "refs/remotes/origin/../release",
		"refs/remotes/origin/release.lock", "/outside/refs/remotes/origin/release",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			repo, fs, _ := namespaceFixture(t)
			original := &os.PathError{Op: "open", Path: path, Err: syscall.EISDIR}
			err := fetchWithRefNamespaceRecovery(repo, namespaceOptions(), func() error { return original },
				func() ([]*plumbing.Reference, error) {
					t.Fatal("invalid error path listed the remote")
					return nil, nil
				})

			if !isRefNamespaceError(err) {
				t.Fatalf("unsafe path did not fail explicitly: %v", err)
			}

			if _, err := fs.Lstat(namespaceDestination + "/old"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRefNamespaceRelativeRepositoryPath(t *testing.T) {
	t.Chdir(t.TempDir())

	repo, err := git.PlainInit("mirror", true)
	if err != nil {
		t.Fatal(err)
	}

	stale := plumbing.NewHashReference(namespaceDestination+"/old", plumbing.NewHash(strings.Repeat("1", 40)))
	if err := repo.Storer.SetReference(stale); err != nil {
		t.Fatal(err)
	}

	replacement := plumbing.NewHashReference(namespaceDestination, namespaceAdvertisement()[0].Hash())
	fetches := 0

	err = fetchWithRefNamespaceRecovery(repo, namespaceOptions(), func() error {
		fetches++

		err := repo.Storer.SetReference(replacement)
		if fetches == 1 {
			if pathErr, ok := errors.AsType[*os.PathError](err); !ok || filepath.IsAbs(pathErr.Path) {
				t.Errorf("first write error = %v, want a relative path error", err)
			}
		}

		return err
	}, func() ([]*plumbing.Reference, error) { return namespaceAdvertisement(), nil })
	if err != nil || fetches != 2 {
		t.Fatalf("relative repository recovery = %v, fetches = %d", err, fetches)
	}

	if got, err := repo.Reference(replacement.Name(), false); err != nil || got.Hash() != replacement.Hash() {
		t.Fatalf("replacement reference = %v, %v", got, err)
	}
}

func TestRefNamespaceUnsupportedStorage(t *testing.T) {
	t.Parallel()

	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatal(err)
	}

	err = fetchWithRefNamespaceRecovery(repo, namespaceOptions(), func() error { return syscall.EISDIR },
		func() ([]*plumbing.Reference, error) {
			t.Fatal("unsupported storage listed the remote")
			return nil, nil
		})

	if !isRefNamespaceError(err) || !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("unsupported storage error = %v", err)
	}
}

type namespaceFaultFS struct {
	billy.Filesystem
	statPath   string
	removePath string
	failure    error
}

func (fs namespaceFaultFS) Lstat(name string) (os.FileInfo, error) {
	if name == fs.statPath {
		return nil, fs.failure
	}

	return fs.Filesystem.Lstat(name)
}

func (fs namespaceFaultFS) Remove(name string) error {
	if name == fs.removePath {
		return fs.failure
	}

	return fs.Filesystem.Remove(name)
}

type namespaceFaultStorage struct {
	storage.Storer
	fs      billy.Filesystem
	failure error
}

func (s namespaceFaultStorage) Filesystem() billy.Filesystem { return s.fs }
func (s namespaceFaultStorage) RemoveReference(_ plumbing.ReferenceName) error {
	return s.failure
}

func TestRefNamespacePropagatesStorageErrors(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{"inspect", "remove file", "remove directory", "remove storage"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()

			repo, fs, original := namespaceFixture(t)
			failure := fmt.Errorf("injected %s: %w", operation, os.ErrPermission)
			fault := namespaceFaultFS{Filesystem: fs, failure: failure}

			switch operation {
			case "inspect":
				fault.statPath = namespaceDestination + "/old"
			case "remove file":
				fault.removePath = namespaceDestination + "/old"
			case "remove directory":
				fault.removePath = namespaceDestination
			}

			storer := storage.Storer(gitfs.NewStorage(fault, cache.NewObjectLRUDefault()))
			if operation == "remove storage" {
				storer = namespaceFaultStorage{Storer: repo.Storer, fs: fs, failure: failure}
			}

			faultRepo, err := git.Open(storer, nil)
			if err != nil {
				t.Fatal(err)
			}

			fetches := 0
			err = fetchWithRefNamespaceRecovery(faultRepo, namespaceOptions(), func() error {
				fetches++
				return original
			}, func() ([]*plumbing.Reference, error) { return namespaceAdvertisement(), nil })

			if !isRefNamespaceError(err) || !errors.Is(err, failure) || fetches != 1 {
				t.Fatalf("storage failure = %v, fetches = %d", err, fetches)
			}
		})
	}
}

func TestRefNamespaceSharedReferenceFilesystem(t *testing.T) {
	t.Parallel()

	repo, common, _ := namespaceFixture(t)

	worktreeRepo, err := git.PlainInit(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}

	local, err := referenceFilesystem(worktreeRepo)
	if err != nil {
		t.Fatal(err)
	}

	fs := dotgit.NewRepositoryFilesystem(local, common)

	shared, err := git.Open(gitfs.NewStorage(fs, cache.NewObjectLRUDefault()), nil)
	if err != nil {
		t.Fatal(err)
	}

	original := &os.PathError{Op: "open", Path: filepath.Join(common.Root(), namespaceDestination), Err: syscall.EISDIR}
	calls := 0

	err = fetchWithRefNamespaceRecovery(shared, namespaceOptions(), func() error {
		calls++
		if calls == 1 {
			return original
		}

		return nil
	}, func() ([]*plumbing.Reference, error) { return namespaceAdvertisement(), nil })
	if err != nil {
		t.Fatal(err)
	}

	if _, err := repo.Reference(namespaceDestination+"/old", false); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Fatalf("common reference was not removed: %v", err)
	}

	if _, err := local.Lstat("HEAD"); err != nil {
		t.Fatalf("worktree HEAD was touched: %v", err)
	}
}

func TestRefNamespacePublicBareFetchPathLock(t *testing.T) {
	for _, focused := range []bool{false, true} {
		t.Run(fmt.Sprintf("focused=%t", focused), func(t *testing.T) {
			originPath, mirrorPath, hash := setupMirrorWithPack(t)

			origin, err := git.PlainOpen(originPath)
			if err != nil {
				t.Fatal(err)
			}

			if err := origin.Storer.SetReference(plumbing.NewHashReference("refs/heads/release", hash)); err != nil {
				t.Fatal(err)
			}

			repo, err := git.PlainOpen(mirrorPath)
			if err != nil {
				t.Fatal(err)
			}

			if err := repo.Storer.SetReference(plumbing.NewHashReference(namespaceDestination+"/old", hash)); err != nil {
				t.Fatal(err)
			}

			unlock := AcquireSharedMirrorLock(mirrorPath)
			done := make(chan error, 1)

			go func() {
				if focused {
					done <- FetchRepositoryReference(repo, originPath, "release", false, transport.ProxyOptions{}, nil, 0)
				} else {
					done <- FetchRepository(repo, originPath, false, transport.ProxyOptions{}, nil, 0)
				}
			}()

			select {
			case err := <-done:
				unlock()
				t.Fatalf("bare fetch completed while a reader held the mirror lock: %v", err)
			case <-time.After(50 * time.Millisecond):
			}

			if _, err := repo.Reference(namespaceDestination+"/old", false); err != nil {
				unlock()
				t.Fatalf("blocked fetch already mutated refs: %v", err)
			}

			unlock()

			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("bare fetch did not finish after reader released lock")
			}

			if err := WithMirrorRead(mirrorPath, func(repo *git.Repository) error {
				got, err := ResolveReferenceCommit(repo, "release")
				if err == nil && got != hash {
					return fmt.Errorf("resolved %s, want %s", got, hash)
				}

				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRefNamespaceMixedPackedAndLoose(t *testing.T) {
	t.Parallel()

	repo, fs, original := namespaceFixture(t)
	if err := repo.Storer.SetReference(plumbing.NewHashReference(
		namespaceDestination+"/packed", plumbing.NewHash(strings.Repeat("3", 40)))); err != nil {
		t.Fatal(err)
	}

	filesystemStorage, ok := repo.Storer.(*gitfs.Storage)
	if !ok {
		t.Fatal("expected filesystem storage")
	}

	if err := filesystemStorage.PackRefs(); err != nil {
		t.Fatal(err)
	}

	// go-git accepts blank lines in packed-refs, so namespace repair must accept them too.
	packed, err := fs.OpenFile("packed-refs", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}

	_, writeErr := packed.Write([]byte("\n"))
	if err := errors.Join(writeErr, packed.Close()); err != nil {
		t.Fatal(err)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(
		namespaceDestination+"/old", plumbing.NewHash(strings.Repeat("1", 40)))); err != nil {
		t.Fatal(err)
	}

	if err := repairRefNamespace(repo, namespaceOptions().RefSpecs, original,
		func() ([]*plumbing.Reference, error) { return namespaceAdvertisement(), nil }); err != nil {
		t.Fatal(err)
	}

	for _, ref := range []plumbing.ReferenceName{namespaceDestination + "/old", namespaceDestination + "/packed"} {
		if _, err := repo.Reference(ref, false); !errors.Is(err, plumbing.ErrReferenceNotFound) {
			t.Fatalf("stale loose/packed reference %s remains: %v", ref, err)
		}
	}

	if _, err := fs.Lstat(namespaceDestination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocking directory remains: %v", err)
	}
}

func TestRefNamespaceBroadRecoveryRepairsAllDestinations(t *testing.T) {
	t.Parallel()

	repo, fs, original := namespaceFixture(t)
	if err := repo.Storer.SetReference(plumbing.NewHashReference(
		"refs/tags/v1/old/deep", plumbing.NewHash(strings.Repeat("3", 40)))); err != nil {
		t.Fatal(err)
	}

	advertised := append(namespaceAdvertisement(),
		plumbing.NewHashReference("refs/tags/v1", plumbing.NewHash(strings.Repeat("4", 40))),
		plumbing.NewHashReference("refs/tags/v1^{}", plumbing.NewHash(strings.Repeat("4", 40))))
	fetches := 0
	err := fetchWithRefNamespaceRecovery(repo,
		&git.FetchOptions{RefSpecs: []config.RefSpec{refSpecAllBranches, refSpecAllTags}},
		func() error {
			fetches++
			if fetches == 1 {
				return original
			}

			for _, name := range []string{namespaceDestination, "refs/tags/v1"} {
				if _, err := fs.Lstat(name); !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("blocking directory %s remains: %v", name, err)
				}
			}

			return nil
		}, func() ([]*plumbing.Reference, error) { return advertised, nil })

	if err != nil || fetches != 2 {
		t.Fatalf("broad repair = %v, fetches = %d", err, fetches)
	}
}

type namespaceBlockingFS struct {
	billy.Filesystem
	blocked chan struct{}
	resume  chan struct{}
}

func (fs namespaceBlockingFS) Remove(name string) error {
	if name == namespaceDestination {
		close(fs.blocked)
		<-fs.resume
	}

	return fs.Filesystem.Remove(name)
}

func TestRefNamespaceReadersWaitForCleanup(t *testing.T) {
	t.Parallel()

	_, fs, original := namespaceFixture(t)
	blocked, resume := make(chan struct{}), make(chan struct{})
	blocking := namespaceBlockingFS{Filesystem: fs, blocked: blocked, resume: resume}

	repo, err := git.Open(gitfs.NewStorage(blocking, cache.NewObjectLRUDefault()), nil)
	if err != nil {
		t.Fatal(err)
	}

	var once sync.Once

	release := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(release)

	writerDone := make(chan error, 1)

	go func() {
		unlock := AcquireExclusiveMirrorLock(fs.Root())
		defer unlock()

		fetches := 0

		writerDone <- fetchWithRefNamespaceRecovery(repo, namespaceOptions(), func() error {
			fetches++
			if fetches == 1 {
				return original
			}

			return repo.Storer.SetReference(plumbing.NewHashReference(
				namespaceDestination, namespaceAdvertisement()[0].Hash()))
		}, func() ([]*plumbing.Reference, error) { return namespaceAdvertisement(), nil })
	}()

	select {
	case <-blocked:
	case err := <-writerDone:
		t.Fatalf("writer did not reach blocking cleanup: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not reach blocking cleanup")
	}

	if _, err := fs.Lstat(namespaceDestination + "/old"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected partially repaired refs while writer holds lock: %v", err)
	}

	readerDone := make(chan error, 1)
	go func() {
		readerDone <- WithMirrorRead(fs.Root(), func(repo *git.Repository) error {
			ref, err := repo.Reference(namespaceDestination, false)
			if err != nil {
				return err
			}

			if ref.Hash() != namespaceAdvertisement()[0].Hash() {
				return errors.New("reader observed the wrong revision after cleanup")
			}

			return nil
		})
	}()

	select {
	case err := <-readerDone:
		t.Fatalf("reader observed partial namespace cleanup: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()

	for _, done := range []chan error{writerDone, readerDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("mirror operation did not finish after cleanup")
		}
	}
}

func TestRefNamespacePackedInspectionFailureDoesNotDelete(t *testing.T) {
	t.Parallel()

	repo, fs, original := namespaceFixture(t)

	hash := plumbing.NewHash(strings.Repeat("3", 40))
	if err := repo.Storer.SetReference(plumbing.NewHashReference(namespaceDestination+"/packed", hash)); err != nil {
		t.Fatal(err)
	}

	filesystemStorage, ok := repo.Storer.(*gitfs.Storage)
	if !ok {
		t.Fatal("expected filesystem storage")
	}

	if err := filesystemStorage.PackRefs(); err != nil {
		t.Fatal(err)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(namespaceDestination+"/old", hash)); err != nil {
		t.Fatal(err)
	}

	fault := namespaceFaultFS{
		Filesystem: fs, statPath: namespaceDestination + "/packed", failure: os.ErrPermission,
	}

	faultRepo, err := git.Open(gitfs.NewStorage(fault, cache.NewObjectLRUDefault()), nil)
	if err != nil {
		t.Fatal(err)
	}

	err = repairRefNamespace(faultRepo, namespaceOptions().RefSpecs, original,
		func() ([]*plumbing.Reference, error) { return namespaceAdvertisement(), nil })
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("packed inspection failure was hidden: %v", err)
	}

	if _, err := fs.Lstat(namespaceDestination + "/old"); err != nil {
		t.Fatalf("inspection failure deleted a previously inspected loose ref: %v", err)
	}
}
