package git

import (
	"fmt"
	"strings"
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
	got, err := GetCommitsBetween(repo, h[0], h[3], 50, nil)
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
	empty, err := GetCommitsBetween(repo, h[3], h[3], 50, nil)
	if err != nil {
		t.Fatalf("GetCommitsBetween equal: %v", err)
	}

	if len(empty) != 0 {
		t.Fatalf("expected 0 commits, got %d", len(empty))
	}

	// cap is honoured
	capped, err := GetCommitsBetween(repo, plumbing.ZeroHash, h[3], 2, nil)
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

	got, err := GetCommitsBetween(repo, h[2], d[1], 50, nil)
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

func TestGetShortestUniqueCommitHash(t *testing.T) {
	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	hashes := commitN(t, wt, 3)
	target := hashes[2].String()

	got, err := GetShortestUniqueCommitHash(repo, target, 1)
	if err != nil {
		t.Fatalf("GetShortestUniqueCommitHash: %v", err)
	}

	for _, hash := range hashes[:2] {
		if strings.HasPrefix(hash.String(), got) {
			t.Fatalf("prefix %q for %s is not unique; also matches %s", got, target, hash)
		}
	}

	if got != target[:len(got)] {
		t.Fatalf("got %q, want prefix of %s", got, target)
	}
}

func TestSharedPrefixLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		first  string
		second string
		want   int
	}{
		{name: "no shared prefix", first: "abcdef", second: "123456", want: 0},
		{name: "shared prefix", first: "abcdef", second: "abc123", want: 3},
		{name: "identical strings", first: "abcdef", second: "abcdef", want: 6},
		{name: "prefix of second", first: "abc", second: "abcdef", want: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sharedPrefixLength(tt.first, tt.second); got != tt.want {
				t.Errorf("sharedPrefixLength(%q, %q) = %d, want %d", tt.first, tt.second, got, tt.want)
			}
		})
	}
}

// commitFileAt writes content to relPath in the worktree and commits it with a fixed commit time.
func commitFileAt(t *testing.T, wt *gogit.Worktree, relPath, content, msg string, when time.Time) plumbing.Hash {
	t.Helper()

	f, err := wt.Filesystem.Create(relPath)
	if err != nil {
		t.Fatalf("create %s: %v", relPath, err)
	}

	if _, err = f.Write([]byte(content)); err != nil {
		t.Fatalf("write %s: %v", relPath, err)
	}

	if err = f.Close(); err != nil {
		t.Fatalf("close %s: %v", relPath, err)
	}

	if _, err = wt.Add(relPath); err != nil {
		t.Fatalf("add %s: %v", relPath, err)
	}

	sig := &object.Signature{Name: "Jane Doe", Email: "jane@example.com", When: when}

	h, err := wt.Commit(msg, &gogit.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatalf("commit %s: %v", msg, err)
	}

	return h
}

// stackPathFilter matches everything below dir, like the filter a compose project produces.
func stackPathFilter(dir string) func(string) bool {
	return func(p string) bool {
		return strings.HasPrefix(p, dir+"/")
	}
}

// A stack only wants the commits that touch its own files, not everything that happened
// in a repository holding several stacks.
func TestGetCommitsBetween_PathFilter(t *testing.T) {
	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	first := commitFileAt(t, wt, "README.md", "initial\n", "initial commit", when)
	stackA := commitFileAt(t, wt, "stacks/a/compose.yaml", "a1\n", "feat(a): first", when.Add(1*time.Minute))
	stackB := commitFileAt(t, wt, "stacks/b/compose.yaml", "b1\n", "feat(b): first", when.Add(2*time.Minute))
	stackBEnv := commitFileAt(t, wt, "stacks/b/.env", "B=1\n", "chore(b): env", when.Add(3*time.Minute))
	stackAEnv := commitFileAt(t, wt, "stacks/a/.env", "A=1\n", "chore(a): env", when.Add(4*time.Minute))

	// Stack A deployed at stackB and now deploys stackAEnv: only its own commit is new.
	// The boundary commit (stackB) touches stack B only, so the path filter drops it from
	// the log. The walk still has to stop there instead of reaching back to stackA.
	got, err := GetCommitsBetween(repo, stackB, stackAEnv, 50, stackPathFilter("stacks/a"))
	if err != nil {
		t.Fatalf("GetCommitsBetween: %v", err)
	}

	if len(got) != 1 || got[0].Hash != stackAEnv.String() {
		t.Fatalf("expected only %s, got %+v", stackAEnv.String(), got)
	}

	// Unfiltered, the same range also carries the commit of stack B.
	unfiltered, err := GetCommitsBetween(repo, stackB, stackAEnv, 50, nil)
	if err != nil {
		t.Fatalf("GetCommitsBetween unfiltered: %v", err)
	}

	if len(unfiltered) != 2 || unfiltered[1].Hash != stackBEnv.String() {
		t.Fatalf("expected [stackAEnv, stackBEnv], got %+v", unfiltered)
	}

	// A stack whose files did not change at all gets an empty changelog.
	none, err := GetCommitsBetween(repo, stackBEnv, stackAEnv, 50, stackPathFilter("stacks/c"))
	if err != nil {
		t.Fatalf("GetCommitsBetween no match: %v", err)
	}

	if len(none) != 0 {
		t.Fatalf("expected no commits, got %+v", none)
	}

	// A filter matching a single file only takes the commits that changed that file.
	single, err := GetCommitsBetween(repo, first, stackAEnv, 50, func(p string) bool {
		return p == "stacks/a/compose.yaml"
	})
	if err != nil {
		t.Fatalf("GetCommitsBetween single file: %v", err)
	}

	if len(single) != 1 || single[0].Hash != stackA.String() {
		t.Fatalf("expected only %s, got %+v", stackA.String(), single)
	}
}

// The cap counts the commits that passed the filter, not the walked ones.
func TestGetCommitsBetween_PathFilterCap(t *testing.T) {
	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first := commitFileAt(t, wt, "README.md", "initial\n", "initial commit", when)

	var wanted []plumbing.Hash

	// Six commits, every second one belongs to stack A.
	for i := range 6 {
		dir := "stacks/b"
		if i%2 == 0 {
			dir = "stacks/a"
		}

		h := commitFileAt(t, wt, dir+"/compose.yaml", fmt.Sprintf("v%d\n", i), fmt.Sprintf("commit %d", i), when.Add(time.Duration(i+1)*time.Minute))
		if dir == "stacks/a" {
			wanted = append(wanted, h)
		}
	}

	got, err := GetCommitsBetween(repo, first, wanted[len(wanted)-1], 2, stackPathFilter("stacks/a"))
	if err != nil {
		t.Fatalf("GetCommitsBetween: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 commits (capped), got %d: %+v", len(got), got)
	}

	if got[0].Hash != wanted[2].String() || got[1].Hash != wanted[1].String() {
		t.Fatalf("expected the two newest stack A commits, got %+v", got)
	}
}
