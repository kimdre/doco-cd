package git

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// captureLogRecords returns a JSON logger and a function that decodes the
// records written to it so far.
func captureLogRecords(t *testing.T) (*slog.Logger, func() []map[string]any) {
	t.Helper()

	var buf bytes.Buffer

	return slog.New(slog.NewJSONHandler(&buf, nil)), func() []map[string]any {
		t.Helper()

		var records []map[string]any

		dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
		for dec.More() {
			var record map[string]any
			if err := dec.Decode(&record); err != nil {
				t.Fatalf("decode log record: %v", err)
			}

			records = append(records, record)
		}

		return records
	}
}

// logField returns the value at the dotted key in record, descending into
// groups, or nil if it is missing.
func logField(record map[string]any, key string) any {
	var value any = record

	for name := range strings.SplitSeq(key, ".") {
		group, ok := value.(map[string]any)
		if !ok {
			return nil
		}

		value = group[name]
	}

	return value
}

// assertElapsedTime fails the test unless record holds a readable elapsed_time
// instead of a raw duration.
func assertElapsedTime(t *testing.T, record map[string]any) {
	t.Helper()

	elapsed, ok := record["elapsed_time"].(string)
	if !ok {
		t.Fatalf("log record elapsed_time = %v, want a duration string", record["elapsed_time"])
	}

	if _, err := time.ParseDuration(elapsed); err != nil {
		t.Fatalf("log record elapsed_time = %q: %v", elapsed, err)
	}

	if _, ok := record["duration"]; ok {
		t.Fatalf("log record still holds the raw duration: %v", record)
	}
}

// setupMirrorWithPacks returns a bare mirror holding exactly packs packfiles,
// one per fetched commit, and the commits that were fetched into it.
func setupMirrorWithPacks(t *testing.T, packs int) (originPath, mirrorPath string, commits []plumbing.Hash) {
	t.Helper()

	originPath, mirrorPath, mainHash := setupMirrorWithPack(t)
	commits = append(commits, mainHash)

	for i := 1; i < packs; i++ {
		commits = append(commits, fetchNewCommitIntoMirror(t, originPath, mirrorPath,
			"content "+strconv.Itoa(i)+"\n", "commit "+strconv.Itoa(i)))
	}

	if got := countPacks(t, mirrorPath); got != packs {
		t.Fatalf("mirror holds %d packs, want %d", got, packs)
	}

	return originPath, mirrorPath, commits
}

func countPacks(t *testing.T, mirrorPath string) int {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(mirrorPath, "objects", "pack", "pack-*.pack"))
	if err != nil {
		t.Fatalf("glob packs: %v", err)
	}

	return len(matches)
}

func openMirror(t *testing.T, mirrorPath string) *gogit.Repository {
	t.Helper()

	repo, err := gogit.PlainOpen(mirrorPath)
	if err != nil {
		t.Fatalf("open mirror: %v", err)
	}

	return repo
}

// readCommits reads every commit in commits along with its full tree, so a
// missing object anywhere in the mirror surfaces as an error.
func readCommits(repo *gogit.Repository, commits []plumbing.Hash) error {
	for _, h := range commits {
		c, err := repo.CommitObject(h)
		if err != nil {
			return err
		}

		tree, err := c.Tree()
		if err != nil {
			return err
		}

		if err := tree.Files().ForEach(func(f *object.File) error {
			_, err := f.Contents()
			return err
		}); err != nil {
			return err
		}
	}

	return nil
}

func captureMirrorPackStats(t *testing.T) func() []MirrorPackStats {
	t.Helper()

	var (
		mu    sync.Mutex
		stats []MirrorPackStats
	)

	SetMirrorPackObserver(func(s MirrorPackStats) {
		mu.Lock()
		defer mu.Unlock()

		stats = append(stats, s)
	})
	t.Cleanup(func() { SetMirrorPackObserver(nil) })

	return func() []MirrorPackStats {
		mu.Lock()
		defer mu.Unlock()

		return append([]MirrorPackStats(nil), stats...)
	}
}

// Not parallel: the pack observer is process-global.
func TestCompactBareMirrorLocked_ConsolidatesPacksAndLooseObjects(t *testing.T) {
	stats := captureMirrorPackStats(t)

	_, mirrorPath, commits := setupMirrorWithPacks(t, mirrorCompactPackThreshold+1)

	repo := openMirror(t, mirrorPath)

	loose := repo.Storer.NewEncodedObject()
	loose.SetType(plumbing.BlobObject)

	w, err := loose.Writer()
	if err != nil {
		t.Fatalf("loose object writer: %v", err)
	}

	if _, err := w.Write([]byte("loose object\n")); err != nil {
		t.Fatalf("write loose object: %v", err)
	}

	_ = w.Close()

	looseHash, err := repo.Storer.SetEncodedObject(loose)
	if err != nil {
		t.Fatalf("store loose object: %v", err)
	}

	log, logRecords := captureLogRecords(t)

	if !compactBareMirrorLocked(log, repo, mirrorPath, "example.com/owner/repo") {
		t.Fatal("compactBareMirrorLocked() = false, want true")
	}

	if got := countPacks(t, mirrorPath); got != 1 {
		t.Fatalf("mirror holds %d packs after compaction, want 1", got)
	}

	fresh := openMirror(t, mirrorPath)

	if err := readCommits(fresh, commits); err != nil {
		t.Fatalf("read commits after compaction: %v", err)
	}

	if err := fresh.Storer.(*filesystem.Storage).ForEachObjectHash(func(h plumbing.Hash) error {
		return errors.New("loose object left behind: " + h.String())
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := fresh.BlobObject(looseHash); err != nil {
		t.Fatalf("formerly loose object missing from the new pack: %v", err)
	}

	got := stats()
	if len(got) != 1 {
		t.Fatalf("observer received %d reports, want 1", len(got))
	}

	want := MirrorPackStats{
		Repository:  "example.com/owner/repo",
		PacksBefore: mirrorCompactPackThreshold + 1,
		PacksAfter:  1,
		Result:      MirrorCompactionCompacted,
	}

	if got[0].Duration <= 0 {
		t.Fatalf("observer duration = %v, want > 0", got[0].Duration)
	}

	got[0].Duration = 0
	if got[0] != want {
		t.Fatalf("observer stats = %+v, want %+v", got[0], want)
	}

	records := logRecords()
	if len(records) != 1 || records[0]["msg"] != "compacted bare mirror packfiles" {
		t.Fatalf("log records = %v, want one compaction record", records)
	}

	record := records[0]

	for key, want := range map[string]float64{
		"packs.before":  mirrorCompactPackThreshold + 1,
		"packs.after":   1,
		"objects.loose": 1,
	} {
		if got := logField(record, key); got != want {
			t.Errorf("log record %s = %v, want %v", key, got, want)
		}
	}

	for _, key := range []string{"objects.total", "size_bytes.before", "size_bytes.after"} {
		if n, ok := logField(record, key).(float64); !ok || n <= 0 {
			t.Errorf("log record %s = %v, want a positive number", key, logField(record, key))
		}
	}

	assertElapsedTime(t, record)
}

func TestCompactBareMirrorLocked_LeavesMirrorAtThresholdAlone(t *testing.T) {
	t.Parallel()

	_, mirrorPath, commits := setupMirrorWithPacks(t, mirrorCompactPackThreshold)

	if compactBareMirrorLocked(discardLog, openMirror(t, mirrorPath), mirrorPath, "example.com/owner/repo") {
		t.Fatal("compactBareMirrorLocked() = true at the threshold, want false")
	}

	if got := countPacks(t, mirrorPath); got != mirrorCompactPackThreshold {
		t.Fatalf("mirror holds %d packs, want %d", got, mirrorCompactPackThreshold)
	}

	if err := readCommits(openMirror(t, mirrorPath), commits); err != nil {
		t.Fatalf("read commits: %v", err)
	}
}

// Not parallel: the pack observer is process-global.
func TestCompactBareMirrorLocked_KeepsPacksWhenCompactionFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only pack directory this test relies on")
	}

	stats := captureMirrorPackStats(t)

	_, mirrorPath, commits := setupMirrorWithPacks(t, mirrorCompactPackThreshold+1)
	packDir := filepath.Join(mirrorPath, "objects", "pack")

	// Writing the consolidated pack needs a temp file in the pack directory.
	if err := os.Chmod(packDir, 0o555); err != nil {
		t.Fatalf("chmod pack dir: %v", err)
	}

	t.Cleanup(func() { _ = os.Chmod(packDir, 0o755) }) //nolint:gosec // restores the default directory mode for TempDir cleanup.

	log, logRecords := captureLogRecords(t)

	if compactBareMirrorLocked(log, openMirror(t, mirrorPath), mirrorPath, "example.com/owner/repo") {
		t.Fatal("compactBareMirrorLocked() = true without writing a pack, want false")
	}

	records := logRecords()
	if len(records) != 1 || records[0]["msg"] != "failed to compact bare mirror packfiles, keeping the existing packs" {
		t.Fatalf("log records = %v, want one compaction failure record", records)
	}

	if logField(records[0], "packs.before") != float64(mirrorCompactPackThreshold+1) || records[0]["retry_delay"] != "1h0m0s" {
		t.Fatalf("log record = %v, want packs.before %d and retry_delay 1h0m0s", records[0], mirrorCompactPackThreshold+1)
	}

	assertElapsedTime(t, records[0])

	if got := countPacks(t, mirrorPath); got != mirrorCompactPackThreshold+1 {
		t.Fatalf("mirror holds %d packs after failed compaction, want %d", got, mirrorCompactPackThreshold+1)
	}

	if err := readCommits(openMirror(t, mirrorPath), commits); err != nil {
		t.Fatalf("read commits after failed compaction: %v", err)
	}

	// A failed mirror is not retried on the next fetch, only after a delay.
	if compactBareMirrorLocked(discardLog, openMirror(t, mirrorPath), mirrorPath, "example.com/owner/repo") {
		t.Fatal("compactBareMirrorLocked() retried right after a failure")
	}

	got := stats()
	if len(got) != 2 || got[0].Result != MirrorCompactionFailed || got[1].Result != "" {
		t.Fatalf("observer stats = %+v, want a %q report followed by one without a compaction", got, MirrorCompactionFailed)
	}
}

// commitGitlink commits a tree on top of origin's main that adds a gitlink to a
// commit origin does not hold, the way a submodule pointer looks to its parent.
func commitGitlink(t *testing.T, originPath string) plumbing.Hash {
	t.Helper()

	originRepo := openMirror(t, originPath)

	head, err := originRepo.Head()
	if err != nil {
		t.Fatalf("origin head: %v", err)
	}

	parent, err := originRepo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("origin head commit: %v", err)
	}

	parentTree, err := parent.Tree()
	if err != nil {
		t.Fatalf("origin head tree: %v", err)
	}

	tree := &object.Tree{Entries: append(append([]object.TreeEntry(nil), parentTree.Entries...), object.TreeEntry{
		Name: "vendored",
		Mode: filemode.Submodule,
		Hash: plumbing.NewHash(strings.Repeat("ab", 20)),
	})}

	treeObj := originRepo.Storer.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		t.Fatalf("encode tree: %v", err)
	}

	treeHash, err := originRepo.Storer.SetEncodedObject(treeObj)
	if err != nil {
		t.Fatalf("store tree: %v", err)
	}

	signature := object.Signature{Name: "compact-test", Email: "compact-test@example.com", When: time.Now()}
	commitObj := originRepo.Storer.NewEncodedObject()

	if err := (&object.Commit{
		Author:       signature,
		Committer:    signature,
		Message:      "add gitlink\n",
		TreeHash:     treeHash,
		ParentHashes: []plumbing.Hash{head.Hash()},
	}).Encode(commitObj); err != nil {
		t.Fatalf("encode commit: %v", err)
	}

	commitHash, err := originRepo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatalf("store commit: %v", err)
	}

	if err := originRepo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), commitHash),
	); err != nil {
		t.Fatalf("advance origin main: %v", err)
	}

	return commitHash
}

// TestCloneOrUpdateBareMirror_CompactsPacksWhileReadersRun drives compaction
// through regular mirror updates while readers keep reading the mirror through
// WithMirrorRead, the way concurrent deployments do. It also checks that the
// handle returned after compaction does not index the deleted packs.
func TestCloneOrUpdateBareMirror_CompactsPacksWhileReadersRun(t *testing.T) {
	t.Parallel()

	originPath, _, mainHash := setupLocalMainRepoAndClone(t)
	mirrorPath := filepath.Join(t.TempDir(), "mirror")
	cloneURL := "file://" + originPath

	update := func() *gogit.Repository {
		t.Helper()

		repo, err := CloneOrUpdateBareMirror(discardLog, cloneURL, MainBranch, mirrorPath,
			false, "", "", "", false, transport.ProxyOptions{}, 0)
		if err != nil {
			t.Fatalf("CloneOrUpdateBareMirror() error = %v", err)
		}

		return repo
	}

	update()

	commits := []plumbing.Hash{mainHash, commitGitlink(t, originPath)}

	update()

	var (
		latest    atomic.Pointer[plumbing.Hash]
		readErr   atomic.Pointer[error]
		stop      = make(chan struct{})
		readersWG sync.WaitGroup
	)

	latest.Store(&commits[len(commits)-1])

	for range 4 {
		readersWG.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}

				want := *latest.Load()

				if err := WithMirrorRead(mirrorPath, func(repo *gogit.Repository) error {
					return readCommits(repo, []plumbing.Hash{want})
				}); err != nil {
					readErr.CompareAndSwap(nil, &err)
					return
				}
			}
		})
	}

	originRepo := openMirror(t, originPath)

	var repo *gogit.Repository

	// The clone wrote one pack and the gitlink fetch a second one; the last of
	// these updates pushes the count past the threshold.
	for i := len(commits); i <= mirrorCompactPackThreshold; i++ {
		h := commitFile(t, originRepo, originPath, "README.md", "update "+strconv.Itoa(i)+"\n", "update "+strconv.Itoa(i))
		commits = append(commits, h)

		repo = update()

		latest.Store(&h)
	}

	close(stop)
	readersWG.Wait()

	if err := readErr.Load(); err != nil {
		t.Fatalf("concurrent mirror read failed: %v", *err)
	}

	if got := countPacks(t, mirrorPath); got != 1 {
		t.Fatalf("mirror holds %d packs after compaction, want 1", got)
	}

	if err := readCommits(repo, commits); err != nil {
		t.Fatalf("read commits through the handle returned after compaction: %v", err)
	}
}
