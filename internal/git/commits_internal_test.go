package git

import (
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// commitN creates n empty commits and returns their hashes oldest-first.
func commitN(t *testing.T, wt *gogit.Worktree, n int) []plumbing.Hash {
	t.Helper()

	hashes := make([]plumbing.Hash, 0, n)
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := range n {
		sig := &object.Signature{Name: "Jane Doe", Email: "jane@example.com", When: when.Add(time.Duration(i) * time.Minute)}

		h, err := wt.Commit("commit "+string(rune('a'+i)), &gogit.CommitOptions{
			AllowEmptyCommits: true,
			Author:            sig,
			Committer:         sig,
		})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}

		hashes = append(hashes, h)
	}

	return hashes
}

func TestGetCommitsBetween(t *testing.T) {
	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	h := commitN(t, wt, 4) // h[0] oldest .. h[3] newest

	// commits after h[0] up to h[3]: h[3], h[2], h[1] (newest first)
	got, err := GetCommitsBetween(repo, h[0], h[3], 50)
	if err != nil {
		t.Fatalf("GetCommitsBetween: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("expected 3 commits, got %d: %+v", len(got), got)
	}

	if got[0].Hash != h[3].String() || got[2].Hash != h[1].String() {
		t.Fatalf("wrong order: %+v", got)
	}

	if got[0].Author != "Jane Doe" || got[0].ShortHash != h[3].String()[:DefaultShortSHALength] {
		t.Fatalf("unexpected fields: %+v", got[0])
	}

	// same old==new -> empty
	empty, err := GetCommitsBetween(repo, h[3], h[3], 50)
	if err != nil {
		t.Fatalf("GetCommitsBetween equal: %v", err)
	}

	if len(empty) != 0 {
		t.Fatalf("expected 0 commits, got %d", len(empty))
	}

	// cap is honoured
	capped, err := GetCommitsBetween(repo, plumbing.ZeroHash, h[3], 2)
	if err != nil {
		t.Fatalf("GetCommitsBetween capped: %v", err)
	}

	if len(capped) != 2 {
		t.Fatalf("expected 2 commits (capped), got %d", len(capped))
	}
}

// A force-push/rebase makes the old tip no longer an ancestor of the new tip.
// The walk must stop at the merge-base and return only the diverged commits,
// not the whole new branch.
func TestGetCommitsBetween_DivergedHistory(t *testing.T) {
	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	h := commitN(t, wt, 3) // a, b(h[1]), oldTip=c(h[2])

	// rewind to b and build a divergent history: d, e
	if err := wt.Checkout(&gogit.CheckoutOptions{Hash: h[1]}); err != nil {
		t.Fatalf("checkout: %v", err)
	}

	d := commitN(t, wt, 2) // d and newTip=e(d[1]) are both parented on b

	got, err := GetCommitsBetween(repo, h[2], d[1], 50)
	if err != nil {
		t.Fatalf("GetCommitsBetween: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 diverged commits, got %d: %+v", len(got), got)
	}

	if got[0].Hash != d[1].String() || got[1].Hash != d[0].String() {
		t.Fatalf("expected [e, d], got %+v", got)
	}
}
