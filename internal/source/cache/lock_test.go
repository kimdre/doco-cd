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
