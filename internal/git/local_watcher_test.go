package git_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"

	"github.com/kimdre/doco-cd/internal/git"
)

const watchSignalTimeout = 3 * time.Second

// waitForSignal blocks until the channel sends or the timeout elapses.
// Returns true if a signal was received in time.
func waitForSignal(t *testing.T, ch <-chan struct{}, timeout time.Duration) bool {
	t.Helper()

	select {
	case _, ok := <-ch:
		return ok
	case <-time.After(timeout):
		return false
	}
}

func TestWatchLocalGitRef_StandardRepo(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "repo")
	repo := initLocalTestRepo(t, srcPath)

	ctx := t.Context()

	watchCh, err := git.WatchLocalGitRef(ctx, "file://"+srcPath, nil)
	if err != nil {
		t.Fatalf("WatchLocalGitRef: %v", err)
	}

	// Make a commit after the watcher is running; the loose ref update should fire.
	commitLocalTestFile(t, repo, srcPath, "change.txt", "hello\n", "watched commit")

	if !waitForSignal(t, watchCh, watchSignalTimeout) {
		t.Fatal("expected watcher signal after commit, got none within timeout")
	}
}

func TestWatchLocalGitRef_BareRepo(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "repo.git")
	if err := os.MkdirAll(srcPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := gogit.PlainInit(srcPath, true); err != nil {
		t.Fatalf("init bare repo: %v", err)
	}

	ctx := t.Context()

	watchCh, err := git.WatchLocalGitRef(ctx, "file://"+srcPath, nil)
	if err != nil {
		t.Fatalf("WatchLocalGitRef: %v", err)
	}

	// Directly write a loose ref to simulate a push arriving.
	refsDir := filepath.Join(srcPath, "refs", "heads")
	if err := os.MkdirAll(refsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fakeHash := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if err := os.WriteFile(filepath.Join(refsDir, "main"), []byte(fakeHash+"\n"), 0o600); err != nil {
		t.Fatalf("write ref: %v", err)
	}

	if !waitForSignal(t, watchCh, watchSignalTimeout) {
		t.Fatal("expected watcher signal after ref update, got none within timeout")
	}
}

func TestWatchLocalGitRef_CancelStopsWatcher(t *testing.T) {
	t.Parallel()

	srcPath := filepath.Join(t.TempDir(), "repo")
	initLocalTestRepo(t, srcPath)

	ctx, cancel := context.WithCancel(context.Background())

	watchCh, err := git.WatchLocalGitRef(ctx, "file://"+srcPath, nil)
	if err != nil {
		t.Fatalf("WatchLocalGitRef: %v", err)
	}

	// Queue a debounced callback, then cancel before its debounce interval
	// elapses. The callback must finish before WatchLocalGitRef closes watchCh.
	if err := os.WriteFile(filepath.Join(srcPath, ".git", "refs", "heads", "watch-cancel"),
		[]byte("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n"), 0o600); err != nil {
		t.Fatalf("write ref: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	cancel()

	// After cancellation the channel should be closed (not just silent).
	select {
	case _, ok := <-watchCh:
		if ok {
			t.Fatal("expected closed channel after cancel, got a value")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed within 2s after cancel")
	}
}

func TestWatchLocalGitRef_NonRepository(t *testing.T) {
	t.Parallel()

	_, err := git.WatchLocalGitRef(context.Background(), "file://"+t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected error for non-repository directory, got nil")
	}
}

func TestWatchLocalGitRef_InvalidURL(t *testing.T) {
	t.Parallel()

	_, err := git.WatchLocalGitRef(context.Background(), "https://github.com/example/repo.git", nil)
	if err == nil {
		t.Fatal("expected error for non-file URL, got nil")
	}
}
