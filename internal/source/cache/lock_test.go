package cache_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

// TestHelperProcess is not a real test; it's re-invoked as a subprocess by
// TestAcquirePathLock_ExcludesConcurrentProcesses to hold the cross-process
// lock for a controlled duration, so exclusivity between two OS processes
// (not just goroutines in one process) can be verified.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_LOCK_HELPER_PROCESS") != "1" {
		return
	}

	path := os.Getenv("LOCK_TEST_PATH")
	logPath := os.Getenv("LOCK_TEST_LOG")

	holdMillis, err := strconv.Atoi(os.Getenv("LOCK_TEST_HOLD_MS"))
	if err != nil {
		t.Fatalf("parse LOCK_TEST_HOLD_MS: %v", err)
	}

	unlock := sourcecache.AcquirePathLock(path)

	appendLockEvent(t, logPath, "start")
	time.Sleep(time.Duration(holdMillis) * time.Millisecond)
	appendLockEvent(t, logPath, "end")
	unlock()
}

func TestAcquirePathLock_ExcludesConcurrentProcesses(t *testing.T) {
	dir := t.TempDir()
	lockTarget := filepath.Join(dir, "repo")
	logPath := filepath.Join(dir, "events.log")

	cmd1 := lockHelperCmd(lockTarget, logPath, 300)
	cmd2 := lockHelperCmd(lockTarget, logPath, 300)

	if err := cmd1.Start(); err != nil {
		t.Fatalf("start process 1: %v", err)
	}

	time.Sleep(50 * time.Millisecond) // give process 1 a head start on acquiring the lock

	if err := cmd2.Start(); err != nil {
		t.Fatalf("start process 2: %v", err)
	}

	if err := cmd1.Wait(); err != nil {
		t.Fatalf("process 1 failed: %v", err)
	}

	if err := cmd2.Wait(); err != nil {
		t.Fatalf("process 2 failed: %v", err)
	}

	events := parseLockEvents(t, logPath)
	if len(events) != 4 {
		t.Fatalf("expected 4 lock events (2 processes x start/end), got %d: %v", len(events), events)
	}

	// Two non-overlapping holds sorted by time must alternate start,end,start,end.
	// Any other order means both processes held the lock at once.
	for i, want := range []string{"start", "end", "start", "end"} {
		if events[i].kind != want {
			t.Fatalf("lock acquisitions overlapped across processes, events (sorted by time) = %v", events)
		}
	}
}

// TestAcquirePathLock_Blocks verifies that acquiring the same key blocks a
// concurrent goroutine until the first holder releases it.
func TestAcquirePathLock_Blocks(t *testing.T) {
	t.Parallel()

	key := filepath.Join(t.TempDir(), "repo")

	unlock := sourcecache.AcquirePathLock(key)
	defer unlock()

	done := make(chan struct{})

	go func() {
		unlock2 := sourcecache.AcquirePathLock(key)
		defer unlock2()

		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("expected second lock acquisition to block, but it succeeded")
	case <-time.After(100 * time.Millisecond):
		// Expected to still be blocked.
	}

	unlock()

	select {
	case <-done:
		// Success.
	case <-time.After(1 * time.Second):
		t.Fatalf("expected second lock acquisition to succeed after release, but it timed out")
	}
}

// TestAcquirePathLock_CanonicalizesSymlinks verifies that a path and a
// symlink pointing at it resolve to the same lock, so two callers naming the
// same physical directory differently still exclude each other.
func TestAcquirePathLock_CanonicalizesSymlinks(t *testing.T) {
	t.Parallel()

	realDir := t.TempDir()

	symlinkPath := filepath.Join(t.TempDir(), "repo")
	if err := os.Symlink(realDir, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	unlock := sourcecache.AcquirePathLock(realDir)
	defer unlock()

	done := make(chan struct{})

	go func() {
		unlock2 := sourcecache.AcquirePathLock(symlinkPath)
		defer unlock2()

		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("expected lock on symlinked path to block while real path is held, but it succeeded")
	case <-time.After(100 * time.Millisecond):
		// Expected to still be blocked.
	}

	unlock()

	select {
	case <-done:
		// Success.
	case <-time.After(1 * time.Second):
		t.Fatalf("expected lock on symlinked path to succeed after release, but it timed out")
	}
}

// TestAcquirePathLock_DifferentPathsDoNotBlock verifies that locks on
// different keys never contend with each other.
func TestAcquirePathLock_DifferentPathsDoNotBlock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	unlock1 := sourcecache.AcquirePathLock(filepath.Join(dir, "repo1"))
	defer unlock1()

	unlock2 := sourcecache.AcquirePathLock(filepath.Join(dir, "repo2"))
	defer unlock2()

	// Both locks acquired above without blocking; nothing further to assert.
}

func TestAcquireSharedGCPathLockFailsWithoutCrossProcessLock(t *testing.T) {
	t.Parallel()

	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write parent file: %v", err)
	}

	unlock, err := sourcecache.AcquireSharedGCPathLock(filepath.Join(parentFile, "repo"))
	if err == nil {
		if unlock != nil {
			unlock()
		}

		t.Fatal("AcquireSharedGCPathLock() succeeded without a usable cross-process lock path")
	}

	if unlock != nil {
		t.Fatal("AcquireSharedGCPathLock() returned an unlock function on failure")
	}
}

// TestAcquirePathLock_UnlockIsIdempotent verifies that calling the returned
// unlock function more than once does not panic.
func TestAcquirePathLock_UnlockIsIdempotent(t *testing.T) {
	t.Parallel()

	unlock := sourcecache.AcquirePathLock(filepath.Join(t.TempDir(), "repo"))

	unlock()
	unlock()
}

// TestAcquireSharedPathLock_AllowsConcurrentReaders verifies that any number
// of AcquireSharedPathLock holders on the same key run concurrently, unlike
// the plain exclusive AcquirePathLock.
func TestAcquireSharedPathLock_AllowsConcurrentReaders(t *testing.T) {
	t.Parallel()

	key := filepath.Join(t.TempDir(), "repo")

	unlock1 := sourcecache.AcquireSharedPathLock(key)
	defer unlock1()

	done := make(chan struct{})

	go func() {
		unlock2 := sourcecache.AcquireSharedPathLock(key)
		defer unlock2()

		close(done)
	}()

	select {
	case <-done:
		// Success: a second shared holder did not block on the first.
	case <-time.After(1 * time.Second):
		t.Fatalf("expected a second AcquireSharedPathLock to succeed immediately, but it blocked")
	}
}

// TestAcquireExclusivePathLock_BlocksSharedHolder verifies that
// AcquireExclusivePathLock excludes a concurrent AcquireSharedPathLock
// holder for the same key, and proceeds once the shared holder releases.
func TestAcquireExclusivePathLock_BlocksSharedHolder(t *testing.T) {
	t.Parallel()

	key := filepath.Join(t.TempDir(), "repo")

	unlockShared := sourcecache.AcquireSharedPathLock(key)

	done := make(chan struct{})

	go func() {
		unlockExclusive := sourcecache.AcquireExclusivePathLock(key)
		defer unlockExclusive()

		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("expected AcquireExclusivePathLock to block while a shared holder is active, but it succeeded")
	case <-time.After(100 * time.Millisecond):
		// Expected to still be blocked.
	}

	unlockShared()

	select {
	case <-done:
		// Success.
	case <-time.After(1 * time.Second):
		t.Fatalf("expected AcquireExclusivePathLock to succeed after the shared holder released, but it timed out")
	}
}

// TestAcquireSharedPathLock_BlocksOnExclusiveHolder verifies the reverse
// direction: AcquireSharedPathLock blocks while an AcquireExclusivePathLock
// holder is active, and proceeds once it releases.
func TestAcquireSharedPathLock_BlocksOnExclusiveHolder(t *testing.T) {
	t.Parallel()

	key := filepath.Join(t.TempDir(), "repo")

	unlockExclusive := sourcecache.AcquireExclusivePathLock(key)

	done := make(chan struct{})

	go func() {
		unlockShared := sourcecache.AcquireSharedPathLock(key)
		defer unlockShared()

		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("expected AcquireSharedPathLock to block while an exclusive holder is active, but it succeeded")
	case <-time.After(100 * time.Millisecond):
		// Expected to still be blocked.
	}

	unlockExclusive()

	select {
	case <-done:
		// Success.
	case <-time.After(1 * time.Second):
		t.Fatalf("expected AcquireSharedPathLock to succeed after the exclusive holder released, but it timed out")
	}
}

// TestAcquireExclusivePathLock_BlocksConcurrentExclusive verifies that two
// AcquireExclusivePathLock holders for the same key still exclude each
// other.
func TestAcquireExclusivePathLock_BlocksConcurrentExclusive(t *testing.T) {
	t.Parallel()

	key := filepath.Join(t.TempDir(), "repo")

	unlock1 := sourcecache.AcquireExclusivePathLock(key)

	done := make(chan struct{})

	go func() {
		unlock2 := sourcecache.AcquireExclusivePathLock(key)
		defer unlock2()

		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("expected second AcquireExclusivePathLock to block, but it succeeded")
	case <-time.After(100 * time.Millisecond):
		// Expected to still be blocked.
	}

	unlock1()

	select {
	case <-done:
		// Success.
	case <-time.After(1 * time.Second):
		t.Fatalf("expected second AcquireExclusivePathLock to succeed after release, but it timed out")
	}
}

func lockHelperCmd(lockPath, logPath string, holdMs int) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$") //nolint:gosec // re-invokes the trusted test binary itself

	cmd.Env = append(os.Environ(),
		"GO_WANT_LOCK_HELPER_PROCESS=1",
		"LOCK_TEST_PATH="+lockPath,
		"LOCK_TEST_LOG="+logPath,
		fmt.Sprintf("LOCK_TEST_HOLD_MS=%d", holdMs),
	)
	cmd.Stderr = os.Stderr

	return cmd
}

type lockEvent struct {
	kind string
	ts   int64
}

func appendLockEvent(t *testing.T, logPath, event string) {
	t.Helper()

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // test-owned temp file
	if err != nil {
		t.Fatalf("open lock event log: %v", err)
	}
	defer f.Close()

	if _, err := fmt.Fprintf(f, "%s %d\n", event, time.Now().UnixNano()); err != nil {
		t.Fatalf("write lock event: %v", err)
	}
}

func parseLockEvents(t *testing.T, logPath string) []lockEvent {
	t.Helper()

	data, err := os.ReadFile(logPath) //nolint:gosec // test-owned temp file
	if err != nil {
		t.Fatalf("read lock event log: %v", err)
	}

	var events []lockEvent

	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}

		var (
			kind string
			ts   int64
		)

		if _, err := fmt.Sscanf(line, "%s %d", &kind, &ts); err != nil {
			t.Fatalf("parse lock event line %q: %v", line, err)
		}

		events = append(events, lockEvent{kind: kind, ts: ts})
	}

	sort.Slice(events, func(i, j int) bool { return events[i].ts < events[j].ts })

	return events
}
