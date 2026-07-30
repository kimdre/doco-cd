package git

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

func TestIsCorruptionError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "reference not found", err: ErrInvalidReference, want: true},
		{name: "error with reference not found message", err: gogit.ErrInvalidReference, want: true},
		{name: "other error", err: ErrMissingAuthToken, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCorruptionError(tt.err); got != tt.want {
				t.Fatalf("IsCorruptionError(%v)=%v want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRepairRepository_RepairsCorruptionModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		corrupt func(t *testing.T, clonePath string)
	}{
		{
			name: "missing local and remote branch references",
			corrupt: func(t *testing.T, clonePath string) {
				t.Helper()

				repo, err := gogit.PlainOpen(clonePath)
				if err != nil {
					t.Fatalf("open clone: %v", err)
				}

				_ = repo.Storer.RemoveReference(plumbing.NewBranchReferenceName("main"))
				_ = repo.Storer.RemoveReference(plumbing.NewRemoteReferenceName(RemoteName, "main"))
			},
		},
		{
			name: "missing refs directories",
			corrupt: func(t *testing.T, clonePath string) {
				t.Helper()

				if err := os.RemoveAll(filepath.Join(clonePath, ".git", "refs", "heads")); err != nil {
					t.Fatalf("remove refs/heads: %v", err)
				}

				if err := os.RemoveAll(filepath.Join(clonePath, ".git", "refs", "remotes")); err != nil {
					t.Fatalf("remove refs/remotes: %v", err)
				}
			},
		},
		{
			name: "broken HEAD plus missing branch",
			corrupt: func(t *testing.T, clonePath string) {
				t.Helper()

				headPath := filepath.Join(clonePath, ".git", "HEAD")
				if err := os.WriteFile(headPath, []byte("ref: refs/heads/does-not-exist\n"), filesystem.PermOwner); err != nil {
					t.Fatalf("write broken HEAD: %v", err)
				}

				repo, err := gogit.PlainOpen(clonePath)
				if err != nil {
					t.Fatalf("open clone: %v", err)
				}

				_ = repo.Storer.RemoveReference(plumbing.NewBranchReferenceName("main"))
				_ = repo.Storer.RemoveReference(plumbing.NewRemoteReferenceName(RemoteName, "main"))
			},
		},
		{
			name: "malformed packed-refs with no loose refs",
			corrupt: func(t *testing.T, clonePath string) {
				t.Helper()

				repo, err := gogit.PlainOpen(clonePath)
				if err != nil {
					t.Fatalf("open clone: %v", err)
				}

				_ = repo.Storer.RemoveReference(plumbing.NewBranchReferenceName("main"))
				_ = repo.Storer.RemoveReference(plumbing.NewRemoteReferenceName(RemoteName, "main"))

				// Remove loose refs so packed-refs becomes the only source.
				_ = os.RemoveAll(filepath.Join(clonePath, ".git", "refs", "heads"))
				_ = os.RemoveAll(filepath.Join(clonePath, ".git", "refs", "remotes"))

				packedRefsPath := filepath.Join(clonePath, ".git", "packed-refs")

				packed := []byte("# pack-refs with: peeled fully-peeled\nthis-line-is-intentionally-malformed\n")
				if err := os.WriteFile(packedRefsPath, packed, filesystem.PermOwner); err != nil {
					t.Fatalf("write malformed packed-refs: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originPath, clonePath, originHash := setupLocalMainRepoAndClone(t)
			tt.corrupt(t, clonePath)

			brokenRepo, err := gogit.PlainOpen(clonePath)
			if err != nil {
				t.Fatalf("open broken repo: %v", err)
			}

			if _, err := GetReferenceSet(brokenRepo, MainBranch); err == nil {
				t.Fatalf("expected corruption precondition, but reference %s was still resolvable", MainBranch)
			}

			repairedRepo, err := RepairRepository(
				clonePath,
				originPath,
				MainBranch,
				false,
				transport.ProxyOptions{},
				nil,
				false,
				0,
				slog.Default(),
			)
			if err != nil {
				t.Fatalf("repair failed: %v", err)
			}

			assertRepoOnMainHash(t, repairedRepo, originHash)
		})
	}
}

func TestUpdateRepository_RepairsCorruptionWithoutDeadlock(t *testing.T) {
	t.Parallel()

	originPath, clonePath, originHash := setupLocalMainRepoAndClone(t)

	repo, err := gogit.PlainOpen(clonePath)
	if err != nil {
		t.Fatalf("open clone: %v", err)
	}

	_ = repo.Storer.RemoveReference(plumbing.NewBranchReferenceName("main"))
	_ = repo.Storer.RemoveReference(plumbing.NewRemoteReferenceName(RemoteName, "main"))

	done := make(chan struct {
		repo *gogit.Repository
		err  error
	}, 1)

	go func() {
		r, e := UpdateRepository(clonePath, originPath, MainBranch, false, transport.ProxyOptions{}, nil, false, 0)
		done <- struct {
			repo *gogit.Repository
			err  error
		}{repo: r, err: e}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("UpdateRepository failed: %v", res.err)
		}

		assertRepoOnMainHash(t, res.repo, originHash)
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateRepository timed out (possible path-lock deadlock during repair)")
	}
}

func TestRepairRepositoryWithMissingPath(t *testing.T) {
	tmpDir := t.TempDir()
	missingPath := filepath.Join(tmpDir, "missing")

	_, err := RepairRepository(
		missingPath,
		filepath.Join(tmpDir, "origin"),
		MainBranch,
		false,
		transport.ProxyOptions{},
		nil,
		false,
		0,
		slog.Default(),
	)
	if err == nil {
		t.Fatal("expected error for missing repository path")
	}
}

func TestRepairRepository_BranchMissingOnRemote(t *testing.T) {
	t.Parallel()

	originPath, clonePath, _ := setupLocalMainRepoAndClone(t)

	missingRef := "refs/heads/does-not-exist"

	_, err := RepairRepository(
		clonePath,
		originPath,
		missingRef,
		false,
		transport.ProxyOptions{},
		nil,
		false,
		0,
		slog.Default(),
	)
	if err == nil {
		t.Fatalf("expected repair to fail for missing remote branch %s", missingRef)
	}

	if !errors.Is(err, ErrCheckoutFailed) && !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("expected checkout/reference failure for missing branch, got: %v", err)
	}

	msg := err.Error()
	if !strings.Contains(msg, "does-not-exist") {
		t.Fatalf("expected error to include missing branch name, got: %v", err)
	}
}

func TestUpdateRepository_BranchMissingOnRemote_SkipsRepair(t *testing.T) {
	t.Parallel()

	originPath, clonePath, _ := setupLocalMainRepoAndClone(t)

	// Sentinel file should survive if no re-clone happens.
	sentinel := filepath.Join(clonePath, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	missingRef := "does-not-exist"

	_, err := UpdateRepository(clonePath, originPath, missingRef, false, transport.ProxyOptions{}, nil, false, 0)
	if err == nil {
		t.Fatalf("expected UpdateRepository to fail for missing remote branch %q", missingRef)
	}

	msg := err.Error()
	if !strings.Contains(msg, "invalid reference") {
		t.Fatalf("expected invalid reference error, got: %v", err)
	}

	if strings.Contains(msg, "failed to re-clone repository during repair") {
		t.Fatalf("expected missing-remote-branch flow to skip repair/re-clone, got: %v", err)
	}

	if _, statErr := os.Stat(sentinel); statErr != nil {
		t.Fatalf("expected sentinel to remain (no re-clone), stat error: %v", statErr)
	}
}

func TestRemoteRefExists(t *testing.T) {
	tmpDir := t.TempDir()

	repo, err := gogit.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	_, err = remoteRefExists(context.Background(), repo, "", nil, MainBranch)
	if err == nil {
		t.Fatal("expected error when remote is missing")
	}
}

func setupLocalMainRepoAndClone(t *testing.T) (originPath string, clonePath string, originMain plumbing.Hash) {
	t.Helper()

	base := t.TempDir()
	originPath = filepath.Join(base, "origin")
	clonePath = filepath.Join(base, "clone")

	originRepo, err := gogit.PlainInit(originPath, false)
	if err != nil {
		t.Fatalf("init origin: %v", err)
	}

	firstHash := commitFile(t, originRepo, originPath, "README.md", "hello\n", "initial commit")

	if err := originRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), firstHash)); err != nil {
		t.Fatalf("set main ref: %v", err)
	}

	if err := originRepo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set HEAD->main: %v", err)
	}

	_ = originRepo.Storer.RemoveReference(plumbing.NewBranchReferenceName("master"))

	originMain = commitFile(t, originRepo, originPath, "README.md", "hello from main\n", "main update")

	clonedRepo, err := CloneRepository(clonePath, originPath, MainBranch, false, transport.ProxyOptions{}, nil, false, 0)
	if err != nil {
		t.Fatalf("clone repo: %v", err)
	}

	assertRepoOnMainHash(t, clonedRepo, originMain)

	return originPath, clonePath, originMain
}

func commitFile(t *testing.T, repo *gogit.Repository, repoPath, relPath, content, msg string) plumbing.Hash {
	t.Helper()

	filePath := filepath.Join(repoPath, relPath)
	if err := os.MkdirAll(filepath.Dir(filePath), filesystem.PermDir); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(filePath), err)
	}

	if err := os.WriteFile(filePath, []byte(content), filesystem.PermOwner); err != nil {
		t.Fatalf("write %s: %v", relPath, err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	if _, err := wt.Add(relPath); err != nil {
		t.Fatalf("git add %s: %v", relPath, err)
	}

	hash, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "repair-test",
			Email: "repair-test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("commit %q: %v", msg, err)
	}

	return hash
}

func assertRepoOnMainHash(t *testing.T, repo *gogit.Repository, expected plumbing.Hash) {
	t.Helper()

	if err := CheckoutRepository(repo, MainBranch, nil, false); err != nil {
		t.Fatalf("checkout %s: %v", MainBranch, err)
	}

	mainRef, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		t.Fatalf("read local main ref: %v", err)
	}

	if mainRef.Hash() != expected {
		t.Fatalf("main hash mismatch: got %s want %s", mainRef.Hash(), expected)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}

	if head.Name() != plumbing.NewBranchReferenceName("main") {
		t.Fatalf("HEAD mismatch: got %s want %s", head.Name(), plumbing.NewBranchReferenceName("main"))
	}

	latest, err := GetLatestCommit(repo, MainBranch)
	if err != nil {
		t.Fatalf("GetLatestCommit(%s): %v", MainBranch, err)
	}

	if latest != expected.String() {
		t.Fatalf("latest commit mismatch: got %s want %s", latest, expected)
	}
}

func BenchmarkIsCorruptionError(b *testing.B) {
	err := errors.New("reference not found")

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = IsCorruptionError(err)
	}
}
