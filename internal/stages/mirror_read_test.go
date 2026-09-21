package stages

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/deploy"
)

// runGit runs a git command and fails the test if it does not succeed.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir

	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=doco-cd",
		"GIT_AUTHOR_EMAIL=doco-cd@example.com",
		"GIT_COMMITTER_NAME=doco-cd",
		"GIT_COMMITTER_EMAIL=doco-cd@example.com",
		// Keep the test hermetic: the developer's own git config must not decide
		// whether these fixtures can be created (e.g. safe.bareRepository=explicit).
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// runGitBare runs a git command against a bare repository. The repository is named
// via --git-dir because a user's global config may set safe.bareRepository=explicit,
// which permits bare repositories only when they are addressed that way.
func runGitBare(t *testing.T, gitDir string, args ...string) {
	t.Helper()

	runGit(t, filepath.Dir(gitDir), append([]string{"--git-dir=" + gitDir}, args...)...)
}

// setupOriginAndMirror builds a local origin repository plus a bare mirror clone of
// it, mimicking the mirrors GitStore maintains.
func setupOriginAndMirror(t *testing.T) (originPath, mirrorPath string) {
	t.Helper()

	base := t.TempDir()
	originPath = filepath.Join(base, "origin")
	mirrorPath = filepath.Join(base, "mirror.git")

	if err := os.MkdirAll(originPath, 0o755); err != nil {
		t.Fatalf("mkdir origin: %v", err)
	}

	runGit(t, originPath, "init", "--initial-branch=main", ".")
	// Background `git gc --auto` runs detached and would still be writing into the
	// repository while the test's temp dir is torn down.
	runGit(t, originPath, "config", "gc.auto", "0")
	writeFile(t, filepath.Join(originPath, "README.md"), "first\n")
	runGit(t, originPath, "add", ".")
	runGit(t, originPath, "commit", "-m", "first commit")

	// Mirror the layout GitStore maintains: a bare repository that tracks the origin
	// under refs/remotes/origin/*, not a --mirror clone (which keeps refs/heads/*).
	runGit(t, base, "init", "--bare", mirrorPath)
	runGitBare(t, mirrorPath, "config", "gc.auto", "0")
	runGitBare(t, mirrorPath, "remote", "add", "origin", originPath)
	runGitBare(t, mirrorPath, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
	// Force the objects into a packfile: a mirror serving loose objects only would
	// not exercise the packfile index cache this test is about.
	runGitBare(t, mirrorPath, "gc", "--aggressive", "--prune=now")

	return originPath, mirrorPath
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newMirrorStageManager(mirrorDir string) *StageManager {
	return &StageManager{
		Repository:   &RepositoryData{MirrorDir: mirrorDir},
		DeployConfig: &deploy.Config{Reference: "main"},
	}
}

// TestStageMirrorReadsSurviveConcurrentFetch reproduces the crash that took down a
// running instance: one job held a repository handle opened during its init stage
// while a second job for the same repository fetched new objects into the shared
// mirror, and the first job's post-deploy read then dereferenced a packfile index
// go-git had never built. Reads now open their own handle per lock region, so the
// advanced mirror must be observed cleanly instead of panicking.
func TestStageMirrorReadsSurviveConcurrentFetch(t *testing.T) {
	originPath, mirrorPath := setupOriginAndMirror(t)

	sm := newMirrorStageManager(mirrorPath)

	firstCommit, err := sm.latestCommitFromMirror()
	if err != nil {
		t.Fatalf("latestCommitFromMirror() before fetch = %v, want nil", err)
	}

	if firstCommit == "" {
		t.Fatal("latestCommitFromMirror() before fetch = \"\", want a commit SHA")
	}

	// Stand in for the concurrent job: advance origin and fetch into the same mirror.
	// fetch.unpackLimit forces the fetched objects into a new packfile, which is what
	// go-git's fetch always does in production and what invalidates a retained handle.
	writeFile(t, filepath.Join(originPath, "README.md"), "second\n")
	runGit(t, originPath, "add", ".")
	runGit(t, originPath, "commit", "-m", "second commit")
	runGitBare(t, mirrorPath, "-c", "fetch.unpackLimit=1", "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")

	packs, err := filepath.Glob(filepath.Join(mirrorPath, "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatalf("glob packs: %v", err)
	}

	if len(packs) < 2 {
		t.Fatalf("mirror has %d packfiles, want at least 2 so the read crosses a pack written after the first read", len(packs))
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("mirror read panicked after a concurrent fetch: %v", r)
		}
	}()

	secondCommit, err := sm.latestCommitFromMirror()
	if err != nil {
		t.Fatalf("latestCommitFromMirror() after fetch = %v, want nil", err)
	}

	if secondCommit == firstCommit {
		t.Fatalf("latestCommitFromMirror() after fetch = %q, want the newly fetched commit", secondCommit)
	}

	// notificationCommitSha walks every commit object to render a short SHA, which is
	// the exact call that segfaulted on the stale handle in production.
	if sha := sm.notificationCommitSha(); sha == "" {
		t.Fatal("notificationCommitSha() = \"\", want a short SHA for the fetched commit")
	}

	// A run that published the first revision must continue reporting that revision
	// even though a later webhook has advanced the shared mirror to secondCommit.
	sm.Repository.Revision = firstCommit
	if sha, want := sm.notificationCommitSha(), firstCommit[:7]; sha != want {
		t.Fatalf("notificationCommitSha() after mirror advanced = %q, want deployed revision %q", sha, want)
	}
}

// TestVerifyMirrorReadableRejectsUnknownMirror ensures a missing mirror path is
// reported rather than silently degrading to an unlocked, unscoped read.
func TestVerifyMirrorReadableRejectsUnknownMirror(t *testing.T) {
	t.Parallel()

	if err := verifyMirrorReadable(""); err == nil {
		t.Fatal("verifyMirrorReadable(\"\") = nil, want an error")
	}
}

// TestHasGitMirror pins the source check that replaced the retained handle.
func TestHasGitMirror(t *testing.T) {
	t.Parallel()

	if newMirrorStageManager("").hasGitMirror() {
		t.Fatal("hasGitMirror() = true for an empty mirror dir, want false")
	}

	if !newMirrorStageManager("/tmp/mirror.git").hasGitMirror() {
		t.Fatal("hasGitMirror() = false for a set mirror dir, want true")
	}
}
