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
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/git"
)

func TestCloneOrUpdateBareMirror_RefNamespaceRefusesUnsafeRepair(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "src")
	origin := initLocalTestRepo(t, srcPath)

	head, err := origin.Head()
	if err != nil {
		t.Fatal(err)
	}

	oldRef := plumbing.NewBranchReferenceName("release/old")
	if err := origin.Storer.SetReference(plumbing.NewHashReference(oldRef, head.Hash())); err != nil {
		t.Fatal(err)
	}

	mirrorPath := filepath.Join(t.TempDir(), "mirror")
	if _, err := git.CloneOrUpdateBareMirror(nil, "file://"+srcPath, oldRef.String(), mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 0); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(mirrorPath, "local-marker")
	if err := os.WriteFile(marker, []byte("preserve existing mirror\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := origin.Storer.RemoveReference(oldRef); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(srcPath, ".git", "refs", "heads", "release")); err != nil {
		t.Fatal(err)
	}

	newHash := commitLocalTestFile(t, origin, srcPath, "new.md", "new\n", "new branch")
	for _, ref := range []plumbing.ReferenceName{"refs/heads/release", "refs/tags/release"} {
		hash := newHash
		if ref.IsTag() {
			hash = head.Hash()
		}

		if err := origin.Storer.SetReference(plumbing.NewHashReference(ref, hash)); err != nil {
			t.Fatal(err)
		}
	}

	blocker := filepath.Join(mirrorPath, "refs", "remotes", "origin", "release", "old")
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	if err := os.Symlink(target, blocker); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer

	log := slog.New(slog.NewTextHandler(&output, nil))

	repo, err := git.CloneOrUpdateBareMirror(log, "file://"+srcPath, "release", mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 0)
	if err == nil || repo != nil || !errors.Is(err, git.ErrFetchFailed) {
		t.Fatalf("unsafe namespace recovery = %v, %v", repo, err)
	}

	if strings.Contains(output.String(), "re-cloning") {
		t.Fatalf("unsafe recovery triggered a re-clone: %s", output.String())
	}

	if got, err := os.Readlink(blocker); err != nil || got != target {
		t.Fatalf("blocking symlink changed: %s, %v", got, err)
	}

	if content, err := os.ReadFile(marker); err != nil || string(content) != "preserve existing mirror\n" {
		t.Fatalf("existing mirror was discarded: %q, %v", content, err)
	}

	if err := git.WithMirrorRead(mirrorPath, func(repo *gogit.Repository) error {
		_, err := repo.Reference("refs/tags/release", false)
		if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return errors.New("failed branch fetch fell through to the same-named tag")
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
