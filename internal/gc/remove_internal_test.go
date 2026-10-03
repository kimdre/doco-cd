package gc

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/source"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// newTestRemover returns a RepositoryRemover whose discovery returns live, and the data mount point it uses.
func newTestRemover(t *testing.T, live map[string][]Usage, discoveryErr error) (*RepositoryRemover, string) {
	t.Helper()

	dataMountPoint := t.TempDir()

	r := NewRepositoryRemover(nil, dataMountPoint, dataMountPoint)
	r.liveUsages = func(context.Context, *docker.ContextRegistry, *slog.Logger, string, string) (map[string][]Usage, error) {
		return live, discoveryErr
	}

	return r, dataMountPoint
}

// makeRepositoryDir creates a repository directory with a mirror, one artifact and a live directory.
func makeRepositoryDir(t *testing.T, dataMountPoint, repoName string, revision store.Revision) string {
	t.Helper()

	repoDir := filepath.Join(dataMountPoint, filepath.FromSlash(repoName))
	makeArtifactDir(t, repoDir, revision)

	for _, sub := range []string{store.MirrorSubdir, filepath.Join(store.LiveSubdir, "default", "stack")} {
		if err := os.MkdirAll(filepath.Join(repoDir, sub), 0o755); err != nil {
			t.Fatalf("create %s: %v", sub, err)
		}
	}

	return repoDir
}

func assertExists(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s should exist, stat err = %v", path, err)
	}
}

func assertNotExists(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s should not exist, stat err = %v", path, err)
	}
}

func TestRepositoryRemover_RemovesUnusedRepository(t *testing.T) {
	t.Parallel()

	const repoName = "github.com/owner/unused-repo"

	// Another repository that is still deployed must not keep this one.
	r, dataMountPoint := newTestRemover(t, map[string][]Usage{
		"owner/other-repo": {{Context: "default", Stack: "other", Revision: "abc", Kind: UsageTarget}},
	}, nil)

	repoDir := makeRepositoryDir(t, dataMountPoint, repoName, "abc")

	result, err := r.RemoveIfUnused(t.Context(), testLogger(), repoName)
	if err != nil {
		t.Fatalf("RemoveIfUnused() error = %v", err)
	}

	if !result.Removed || result.Reason != RemovalReasonUnused {
		t.Fatalf("RemoveIfUnused() = %+v, want removed as unused", result)
	}

	assertNotExists(t, repoDir)

	// The lock files are siblings of the repository directory. Other processes can still
	// wait on them, so they must stay. They also keep the parent directory in place.
	assertExists(t, repoDir+".lock")
	assertExists(t, repoDir+".gc-use.lock")
}

func TestRepositoryRemover_KeepsRepositoryInUse(t *testing.T) {
	t.Parallel()

	const repoName = "github.com/owner/shared-repo"

	tests := []struct {
		name  string
		key   string
		usage Usage
	}{
		{
			name:  "target revision on another context",
			key:   "owner/shared-repo",
			usage: Usage{Context: "remote", Stack: "mosquito", Revision: "sha1", Kind: UsageTarget},
		},
		{
			name:  "pinned revision",
			key:   "github.com/owner/shared-repo",
			usage: Usage{Context: "default", Stack: "web", Revision: "sha0", Kind: UsagePinned},
		},
		{
			name:  "config revision",
			key:   "github.com/owner/shared-repo",
			usage: Usage{Context: "default", Stack: "app", Revision: "cfg1", Kind: UsageConfig},
		},
		{
			name:  "deployment without config revision metadata",
			key:   "owner/shared-repo",
			usage: Usage{Context: "default", Stack: "legacy", Revision: allRevisions, Kind: UsageConfig},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, dataMountPoint := newTestRemover(t, map[string][]Usage{tt.key: {tt.usage}}, nil)
			repoDir := makeRepositoryDir(t, dataMountPoint, repoName, "sha2")

			result, err := r.RemoveIfUnused(t.Context(), testLogger(), repoName)
			if err != nil {
				t.Fatalf("RemoveIfUnused() error = %v", err)
			}

			if result.Removed || result.Reason != RemovalReasonInUse {
				t.Fatalf("RemoveIfUnused() = %+v, want kept as in use", result)
			}

			if len(result.Usages) != 1 || result.Usages[0] != tt.usage {
				t.Fatalf("RemoveIfUnused() usages = %+v, want [%+v]", result.Usages, tt.usage)
			}

			assertExists(t, filepath.Join(repoDir, store.ArtifactsSubdir, "sha2"))
			assertExists(t, filepath.Join(repoDir, store.MirrorSubdir))
			assertExists(t, filepath.Join(repoDir, store.LiveSubdir))
		})
	}
}

func TestRepositoryRemover_KeepsRepositoryWithInFlightArtifact(t *testing.T) {
	t.Parallel()

	const repoName = "github.com/owner/inflight-remove-repo"

	r, dataMountPoint := newTestRemover(t, nil, nil)
	repoDir := makeRepositoryDir(t, dataMountPoint, repoName, "new")

	release := source.MarkInFlight(repoName, "new")
	defer release()

	result, err := r.RemoveIfUnused(t.Context(), testLogger(), repoName)
	if err != nil {
		t.Fatalf("RemoveIfUnused() error = %v", err)
	}

	if result.Removed || result.Reason != RemovalReasonInFlight {
		t.Fatalf("RemoveIfUnused() = %+v, want kept as in flight", result)
	}

	assertExists(t, repoDir)
}

func TestRepositoryRemover_KeepsRepositoryWhileGCLockIsHeld(t *testing.T) {
	t.Parallel()

	const repoName = "github.com/owner/locked-repo"

	discovered := false
	r, dataMountPoint := newTestRemover(t, nil, nil)
	r.liveUsages = func(context.Context, *docker.ContextRegistry, *slog.Logger, string, string) (map[string][]Usage, error) {
		discovered = true
		return nil, nil
	}

	repoDir := makeRepositoryDir(t, dataMountPoint, repoName, "abc")

	// A deployment of the repository holds the shared GC lock until its resources are labeled.
	unlock, err := sourcecache.AcquireSharedGCPathLock(repoDir)
	if err != nil {
		t.Fatalf("AcquireSharedGCPathLock() error = %v", err)
	}
	defer unlock()

	result, err := r.RemoveIfUnused(t.Context(), testLogger(), repoName)
	if err != nil {
		t.Fatalf("RemoveIfUnused() error = %v", err)
	}

	if result.Removed || result.Reason != RemovalReasonLocked {
		t.Fatalf("RemoveIfUnused() = %+v, want kept as locked", result)
	}

	if discovered {
		t.Fatal("RemoveIfUnused() inspected Docker contexts without the GC lock")
	}

	assertExists(t, repoDir)
}

func TestRepositoryRemover_KeepsRepositoryWhenDiscoveryFails(t *testing.T) {
	t.Parallel()

	const repoName = "github.com/owner/discovery-repo"

	r, dataMountPoint := newTestRemover(t, nil, errors.New("context unreachable"))
	repoDir := makeRepositoryDir(t, dataMountPoint, repoName, "abc")

	result, err := r.RemoveIfUnused(t.Context(), testLogger(), repoName)
	if err == nil {
		t.Fatal("RemoveIfUnused() error = nil, want discovery error")
	}

	if result.Removed || result.Reason != RemovalReasonDiscoveryFailed {
		t.Fatalf("RemoveIfUnused() = %+v, want kept as discovery failed", result)
	}

	assertExists(t, repoDir)
}

func TestRepositoryRemover_KeepsRepositoryWithoutContextRegistry(t *testing.T) {
	t.Parallel()

	const repoName = "github.com/owner/no-contexts-repo"

	dataMountPoint := t.TempDir()
	r := NewRepositoryRemover(nil, dataMountPoint, dataMountPoint)
	repoDir := makeRepositoryDir(t, dataMountPoint, repoName, "abc")

	result, err := r.RemoveIfUnused(t.Context(), testLogger(), repoName)
	if !errors.Is(err, errNoContexts) {
		t.Fatalf("RemoveIfUnused() error = %v, want %v", err, errNoContexts)
	}

	if result.Removed || result.Reason != RemovalReasonDiscoveryFailed {
		t.Fatalf("RemoveIfUnused() = %+v, want kept as discovery failed", result)
	}

	assertExists(t, repoDir)
}

func TestRepositoryRemover_MissingRepository(t *testing.T) {
	t.Parallel()

	r, dataMountPoint := newTestRemover(t, nil, nil)

	result, err := r.RemoveIfUnused(t.Context(), testLogger(), "github.com/owner/missing-repo")
	if err != nil {
		t.Fatalf("RemoveIfUnused() error = %v", err)
	}

	if result.Removed || result.Reason != RemovalReasonNotFound {
		t.Fatalf("RemoveIfUnused() = %+v, want not found", result)
	}

	assertExists(t, dataMountPoint)
}

func TestRepositoryRemover_RejectsInvalidRepositoryNames(t *testing.T) {
	t.Parallel()

	for _, repoName := range []string{"", ".", "../outside", "owner/../../outside"} {
		t.Run(repoName, func(t *testing.T) {
			t.Parallel()

			r, dataMountPoint := newTestRemover(t, nil, nil)
			marker := filepath.Join(dataMountPoint, "marker")

			if err := os.WriteFile(marker, nil, 0o600); err != nil {
				t.Fatalf("write marker: %v", err)
			}

			result, err := r.RemoveIfUnused(t.Context(), testLogger(), repoName)
			if err == nil {
				t.Fatalf("RemoveIfUnused(%q) error = nil, want error", repoName)
			}

			if result.Removed {
				t.Fatalf("RemoveIfUnused(%q) = %+v, want kept", repoName, result)
			}

			assertExists(t, marker)
		})
	}
}

func TestRepositoryRemover_RemoveUnusedProcessesEveryRepository(t *testing.T) {
	t.Parallel()

	r, dataMountPoint := newTestRemover(t, map[string][]Usage{
		"owner/used": {{Context: "default", Stack: "used", Revision: "abc", Kind: UsageTarget}},
	}, nil)

	usedDir := makeRepositoryDir(t, dataMountPoint, "github.com/owner/used", "abc")
	unusedDir := makeRepositoryDir(t, dataMountPoint, "github.com/owner/unused", "abc")

	r.RemoveUnused(t.Context(), testLogger(), []string{"github.com/owner/used", "github.com/owner/unused"})

	assertExists(t, usedDir)
	assertNotExists(t, unusedDir)
}

func TestRepositoryRemover_RemoveEmptyParents(t *testing.T) {
	t.Parallel()

	dataMountPoint := t.TempDir()
	r := NewRepositoryRemover(nil, dataMountPoint, dataMountPoint)

	// "host/owner" only held the removed repository, so it is removed too.
	repoDir := filepath.Join(dataMountPoint, "host", "owner", "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("create repository dir: %v", err)
	}

	// "other-host" holds another repository and must stay.
	otherRepoDir := filepath.Join(dataMountPoint, "other-host", "owner", "other")
	if err := os.MkdirAll(otherRepoDir, 0o755); err != nil {
		t.Fatalf("create other repository dir: %v", err)
	}

	siblingRepoDir := filepath.Join(dataMountPoint, "other-host", "owner", "repo")
	if err := os.MkdirAll(siblingRepoDir, 0o755); err != nil {
		t.Fatalf("create sibling repository dir: %v", err)
	}

	for _, dir := range []string{repoDir, siblingRepoDir} {
		if err := os.Remove(dir); err != nil {
			t.Fatalf("remove %s: %v", dir, err)
		}

		r.removeEmptyParents(dir)
	}

	assertNotExists(t, filepath.Join(dataMountPoint, "host"))
	assertExists(t, otherRepoDir)
	assertExists(t, dataMountPoint)
}
