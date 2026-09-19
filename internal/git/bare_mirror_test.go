package git_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/git"
)

func TestCloneOrUpdateBareMirror_ClonesBareAndFetchesRef(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	mirrorPath := filepath.Join(t.TempDir(), "mirror")

	repo, err := git.CloneOrUpdateBareMirror(nil, "file://"+srcPath, git.MainBranch, mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 0)
	if err != nil {
		t.Fatalf("CloneOrUpdateBareMirror() error = %v", err)
	}

	if _, err := repo.Worktree(); !errors.Is(err, gogit.ErrIsBareRepository) {
		t.Fatalf("mirror Worktree() error = %v, want %v", err, gogit.ErrIsBareRepository)
	}

	if _, err := os.Stat(filepath.Join(mirrorPath, ".git")); !os.IsNotExist(err) {
		t.Fatalf("expected no .git subdirectory in bare mirror, stat err = %v", err)
	}

	sha, err := git.GetLatestCommit(repo, git.MainBranch)
	if err != nil {
		t.Fatalf("GetLatestCommit() error = %v", err)
	}

	if sha == "" {
		t.Fatal("GetLatestCommit() returned an empty SHA")
	}
}

func TestCloneOrUpdateBareMirror_FetchesNewCommitsOnUpdate(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	srcRepo := initLocalTestRepo(t, srcPath)

	mirrorPath := filepath.Join(t.TempDir(), "mirror")

	repo, err := git.CloneOrUpdateBareMirror(nil, "file://"+srcPath, git.MainBranch, mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 0)
	if err != nil {
		t.Fatalf("initial CloneOrUpdateBareMirror() error = %v", err)
	}

	first, err := git.GetLatestCommit(repo, git.MainBranch)
	if err != nil {
		t.Fatalf("GetLatestCommit() error = %v", err)
	}

	commitLocalTestFile(t, srcRepo, srcPath, "CHANGED.md", "changed\n", "second commit")

	repo, err = git.CloneOrUpdateBareMirror(nil, "file://"+srcPath, git.MainBranch, mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 0)
	if err != nil {
		t.Fatalf("update CloneOrUpdateBareMirror() error = %v", err)
	}

	second, err := git.GetLatestCommit(repo, git.MainBranch)
	if err != nil {
		t.Fatalf("GetLatestCommit() after update error = %v", err)
	}

	if first == second {
		t.Fatal("GetLatestCommit() returned the same revision after a new commit was pushed")
	}

	// The mirror must still be bare after an update, not just after the initial clone.
	if _, err := repo.Worktree(); !errors.Is(err, gogit.ErrIsBareRepository) {
		t.Fatalf("mirror Worktree() error after update = %v, want %v", err, gogit.ErrIsBareRepository)
	}
}

func TestCloneOrUpdateBareMirror_RequiresAuthForPrivate(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	mirrorPath := filepath.Join(t.TempDir(), "mirror")

	_, err := git.CloneOrUpdateBareMirror(nil, "file://"+srcPath, git.MainBranch, mirrorPath,
		true, "", "", "", false, transport.ProxyOptions{}, 0)
	if !errors.Is(err, git.ErrMissingAuthToken) {
		t.Fatalf("error = %v, want %v", err, git.ErrMissingAuthToken)
	}
}

func TestCloneOrUpdateBareMirror_InvalidReference(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	mirrorPath := filepath.Join(t.TempDir(), "mirror")

	_, err := git.CloneOrUpdateBareMirror(nil, "file://"+srcPath, "refs/heads/does-not-exist", mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 0)
	if !errors.Is(err, git.ErrInvalidReference) {
		t.Fatalf("error = %v, want %v", err, git.ErrInvalidReference)
	}
}

// TestCloneOrUpdateBareMirror_ReadOnlyHelpersWorkAgainstBareMirror confirms the
// read-only helpers GitStore.Resolve/Publish depend on (GetLatestCommit,
// GetChangedFilesBetweenCommits, ResolveReferenceCommit) work correctly
// against a bare mirror, with no working tree present at all.
func TestCloneOrUpdateBareMirror_ReadOnlyHelpersWorkAgainstBareMirror(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	srcRepo := initLocalTestRepo(t, srcPath)

	mirrorPath := filepath.Join(t.TempDir(), "mirror")

	repo, err := git.CloneOrUpdateBareMirror(nil, "file://"+srcPath, git.MainBranch, mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 0)
	if err != nil {
		t.Fatalf("CloneOrUpdateBareMirror() error = %v", err)
	}

	first, err := git.GetLatestCommit(repo, git.MainBranch)
	if err != nil {
		t.Fatalf("GetLatestCommit() error = %v", err)
	}

	commitLocalTestFile(t, srcRepo, srcPath, "CHANGED.md", "changed\n", "second commit")

	repo, err = git.CloneOrUpdateBareMirror(nil, "file://"+srcPath, git.MainBranch, mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 0)
	if err != nil {
		t.Fatalf("update CloneOrUpdateBareMirror() error = %v", err)
	}

	second, err := git.GetLatestCommit(repo, git.MainBranch)
	if err != nil {
		t.Fatalf("GetLatestCommit() after update error = %v", err)
	}

	firstHash, err := git.ResolveReferenceCommit(repo, first)
	if err != nil {
		t.Fatalf("ResolveReferenceCommit(first) error = %v", err)
	}

	secondHash, err := git.ResolveReferenceCommit(repo, second)
	if err != nil {
		t.Fatalf("ResolveReferenceCommit(second) error = %v", err)
	}

	changed, err := git.GetChangedFilesBetweenCommits(repo, firstHash, secondHash)
	if err != nil {
		t.Fatalf("GetChangedFilesBetweenCommits() error = %v", err)
	}

	found := false

	for _, cf := range changed {
		if cf.To != nil && cf.To.Path() == "CHANGED.md" {
			found = true
			break
		}
	}

	if !found {
		t.Fatalf("GetChangedFilesBetweenCommits() = %+v, want an entry for CHANGED.md", changed)
	}
}

// TestCloneOrUpdateBareMirror_ReclonesOnDepthChange exercises the shallow
// depth transition against the real network fixture repository: local
// file:// repositories never go shallow (effectiveDepth forces depth to 0
// for them, matching CloneRepository's own behavior), so this needs a
// transport that actually supports git's shallow capability.
func TestCloneOrUpdateBareMirror_ReclonesOnDepthChange(t *testing.T) {
	t.Parallel()

	mirrorPath := filepath.Join(t.TempDir(), "mirror")

	if _, err := git.CloneOrUpdateBareMirror(nil, cloneUrlTest, git.MainBranch, mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 0); err != nil {
		t.Fatalf("full clone CloneOrUpdateBareMirror() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(mirrorPath, "shallow")); !os.IsNotExist(err) {
		t.Fatalf("expected no shallow file after a full clone, stat err = %v", err)
	}

	// A subsequent call requesting a shallow depth must re-clone rather than
	// leave the existing full mirror in place.
	repo, err := git.CloneOrUpdateBareMirror(nil, cloneUrlTest, git.MainBranch, mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 1)
	if err != nil {
		t.Fatalf("shallow re-clone CloneOrUpdateBareMirror() error = %v", err)
	}

	if _, err := repo.Worktree(); !errors.Is(err, gogit.ErrIsBareRepository) {
		t.Fatalf("mirror Worktree() error after re-clone = %v, want %v", err, gogit.ErrIsBareRepository)
	}

	if _, err := os.Stat(filepath.Join(mirrorPath, "shallow")); err != nil {
		t.Fatalf("expected a shallow file after re-cloning with depth=1, stat err = %v", err)
	}
}

// TestCloneOrUpdateBareMirror_LocalFileURL_BareSourceRepository confirms
// GitStore's bare mirror can fetch from a source that is itself a bare
// repository over the in-process file:// transport (localRepoLoader's
// "config file at root" case in local_transport.go).
func TestCloneOrUpdateBareMirror_LocalFileURL_BareSourceRepository(t *testing.T) {
	t.Parallel()

	// Populate a bare repository by cloning a normal one into it, mirroring
	// TestCloneRepository_LocalFileURL_BareRepository's setup.
	srcPath := filepath.Join(t.TempDir(), "src")
	initLocalTestRepo(t, srcPath)

	barePath := filepath.Join(t.TempDir(), "bare.git")
	if _, err := gogit.PlainClone(barePath, true, &gogit.CloneOptions{URL: "file://" + srcPath}); err != nil {
		t.Fatalf("failed to create bare test repo: %v", err)
	}

	mirrorPath := filepath.Join(t.TempDir(), "mirror")

	repo, err := git.CloneOrUpdateBareMirror(nil, "file://"+barePath, git.MainBranch, mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 0)
	if err != nil {
		t.Fatalf("CloneOrUpdateBareMirror() from bare source error = %v", err)
	}

	if _, err := git.GetLatestCommit(repo, git.MainBranch); err != nil {
		t.Fatalf("GetLatestCommit() error = %v", err)
	}
}

// TestCloneOrUpdateBareMirror_LocalFileURL_IgnoresShallowDepth confirms a
// requested depth against a file:// source is ignored rather than failing,
// on both the initial clone and a subsequent update - mirroring
// TestCloneRepository_LocalFileURL_IgnoresShallowDepth for the bare mirror.
func TestCloneOrUpdateBareMirror_LocalFileURL_IgnoresShallowDepth(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	srcRepo := initLocalTestRepo(t, srcPath)
	commitLocalTestFile(t, srcRepo, srcPath, "SECOND.md", "second\n", "second commit")
	commitLocalTestFile(t, srcRepo, srcPath, "THIRD.md", "third\n", "third commit")

	mirrorPath := filepath.Join(t.TempDir(), "mirror")

	if _, err := git.CloneOrUpdateBareMirror(nil, "file://"+srcPath, git.MainBranch, mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 1); err != nil {
		t.Fatalf("CloneOrUpdateBareMirror() with depth error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(mirrorPath, "shallow")); !os.IsNotExist(err) {
		t.Fatalf("mirror of local repository must not be shallow, stat shallow file err = %v", err)
	}

	// A subsequent depth-limited update must not re-clone in a loop or fail.
	if _, err := git.CloneOrUpdateBareMirror(nil, "file://"+srcPath, git.MainBranch, mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 1); err != nil {
		t.Fatalf("update CloneOrUpdateBareMirror() with depth error = %v", err)
	}
}

// TestCloneOrUpdateBareMirror_RepairsCorruptionModes verifies that
// CloneOrUpdateBareMirror recovers by re-cloning when the on-disk bare
// mirror's refs are corrupted in ways that surface as errors during fetch,
// mirroring modes previously covered against the checkout-based repair path.
// Success is asserted both by the resulting commit hash and by observing the
// repair log message, since some corruption modes are otherwise silently
// self-healed by a plain fetch without ever exercising the repair path.
func TestCloneOrUpdateBareMirror_RepairsCorruptionModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		corrupt func(t *testing.T, mirrorPath string)
	}{
		{
			name: "empty local branch ref file",
			corrupt: func(t *testing.T, mirrorPath string) {
				t.Helper()

				refPath := filepath.Join(mirrorPath, "refs", "heads", "main")
				if err := os.WriteFile(refPath, []byte(""), 0o600); err != nil {
					t.Fatalf("write empty ref file: %v", err)
				}
			},
		},
		{
			name: "empty remote-tracking ref file",
			corrupt: func(t *testing.T, mirrorPath string) {
				t.Helper()

				refPath := filepath.Join(mirrorPath, "refs", "remotes", "origin", "main")
				if err := os.WriteFile(refPath, []byte(""), 0o600); err != nil {
					t.Fatalf("write empty ref file: %v", err)
				}
			},
		},
		{
			name: "malformed packed-refs with no loose refs",
			corrupt: func(t *testing.T, mirrorPath string) {
				t.Helper()

				repo, err := gogit.PlainOpen(mirrorPath)
				if err != nil {
					t.Fatalf("open mirror: %v", err)
				}

				if err := repo.Storer.RemoveReference(git.MainBranch); err != nil {
					t.Fatalf("remove main ref: %v", err)
				}

				_ = os.RemoveAll(filepath.Join(mirrorPath, "refs", "heads"))

				packed := []byte("# pack-refs with: peeled fully-peeled\nthis-line-is-intentionally-malformed\n")
				if err := os.WriteFile(filepath.Join(mirrorPath, "packed-refs"), packed, 0o600); err != nil {
					t.Fatalf("write malformed packed-refs: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srcPath := filepath.Join(t.TempDir(), "src")
			initLocalTestRepo(t, srcPath)

			mirrorPath := filepath.Join(t.TempDir(), "mirror")

			repo, err := git.CloneOrUpdateBareMirror(nil, "file://"+srcPath, git.MainBranch, mirrorPath,
				false, "", "", "", false, transport.ProxyOptions{}, 0)
			if err != nil {
				t.Fatalf("initial CloneOrUpdateBareMirror() error = %v", err)
			}

			originHash, err := git.GetLatestCommit(repo, git.MainBranch)
			if err != nil {
				t.Fatalf("GetLatestCommit() error = %v", err)
			}

			tt.corrupt(t, mirrorPath)

			var logBuf bytes.Buffer

			log := slog.New(slog.NewTextHandler(&logBuf, nil))

			repairedRepo, err := git.CloneOrUpdateBareMirror(log, "file://"+srcPath, git.MainBranch, mirrorPath,
				false, "", "", "", false, transport.ProxyOptions{}, 0)
			if err != nil {
				t.Fatalf("repair via CloneOrUpdateBareMirror() error = %v", err)
			}

			if !strings.Contains(logBuf.String(), "bare mirror corruption detected") {
				t.Fatalf("expected corruption repair to be triggered, log output = %q", logBuf.String())
			}

			repairedHash, err := git.GetLatestCommit(repairedRepo, git.MainBranch)
			if err != nil {
				t.Fatalf("GetLatestCommit() after repair error = %v", err)
			}

			if repairedHash != originHash {
				t.Fatalf("repaired mirror commit = %s, want %s", repairedHash, originHash)
			}
		})
	}
}
