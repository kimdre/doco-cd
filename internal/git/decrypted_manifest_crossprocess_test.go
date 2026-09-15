package git_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kimdre/doco-cd/internal/git"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

// TestManifestHelperProcess is not a real test; it's re-invoked as a
// subprocess by TestWriteDecryptedFilesManifest_SurvivesConcurrentProcesses.
// It repeatedly writes one file to the manifest for repoPath, taking the same
// cross-process source-path lock production callers (e.g. LoadCompose) hold
// around WriteDecryptedFilesManifest, so this exercises the exact lock+write
// pattern used in production across two real OS processes.
func TestManifestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_MANIFEST_HELPER_PROCESS") != "1" {
		return
	}

	repoPath := os.Getenv("MANIFEST_TEST_REPO_PATH")
	prefix := os.Getenv("MANIFEST_TEST_FILE_PREFIX")

	iterations, err := strconv.Atoi(os.Getenv("MANIFEST_TEST_ITERATIONS"))
	if err != nil {
		t.Fatalf("parse MANIFEST_TEST_ITERATIONS: %v", err)
	}

	for i := range iterations {
		unlock := sourcecache.AcquirePathLock(repoPath)

		file := fmt.Sprintf("%s/file-%d.env", prefix, i)
		if err := git.WriteDecryptedFilesManifest(repoPath, []string{file}); err != nil {
			unlock()
			t.Fatalf("WriteDecryptedFilesManifest(%q): %v", file, err)
		}

		unlock()
	}
}

// TestWriteDecryptedFilesManifest_SurvivesConcurrentProcesses proves the fix
// for the production bug where two doco-cd instances sharing one data volume
// (e.g. a self-update setup) raced on the manifest's read-merge-write cycle:
// each process's AcquirePathLock only excluded goroutines in its own process,
// so concurrent writes from the two instances silently dropped each other's
// entries. With the cross-process flock in AcquirePathLock, the final
// manifest must contain every file written by both processes.
func TestWriteDecryptedFilesManifest_SurvivesConcurrentProcesses(t *testing.T) {
	repoPath := t.TempDir()
	repo := initLocalTestRepo(t, repoPath)

	const iterationsPerProcess = 25

	cmdA := manifestHelperCmd(repoPath, "stack-a", iterationsPerProcess)
	cmdB := manifestHelperCmd(repoPath, "stack-b", iterationsPerProcess)

	if err := cmdA.Start(); err != nil {
		t.Fatalf("start process A: %v", err)
	}

	if err := cmdB.Start(); err != nil {
		t.Fatalf("start process B: %v", err)
	}

	if err := cmdA.Wait(); err != nil {
		t.Fatalf("process A failed: %v", err)
	}

	if err := cmdB.Wait(); err != nil {
		t.Fatalf("process B failed: %v", err)
	}

	recorded := git.ReadDecryptedFilesManifest(repo)

	if recorded.Len() != 2*iterationsPerProcess {
		t.Fatalf("manifest has %d entries, want %d (some entries were lost to a cross-process race): %v",
			recorded.Len(), 2*iterationsPerProcess, recorded)
	}

	for _, prefix := range []string{"stack-a", "stack-b"} {
		for i := range iterationsPerProcess {
			file := fmt.Sprintf("%s/file-%d.env", prefix, i)
			if !recorded.Contains(file) {
				t.Errorf("manifest missing %q, dropped by a cross-process race", file)
			}
		}
	}
}

func manifestHelperCmd(repoPath, filePrefix string, iterations int) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestManifestHelperProcess$") //nolint:gosec // re-invokes the trusted test binary itself

	cmd.Env = append(os.Environ(),
		"GO_WANT_MANIFEST_HELPER_PROCESS=1",
		"MANIFEST_TEST_REPO_PATH="+filepath.Clean(repoPath),
		"MANIFEST_TEST_FILE_PREFIX="+filePrefix,
		fmt.Sprintf("MANIFEST_TEST_ITERATIONS=%d", iterations),
	)
	cmd.Stderr = os.Stderr

	return cmd
}
