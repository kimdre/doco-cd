package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/common/types/set"
)

// touchArtifact publishes revision under baseDir and backdates its
// directory's modification time to age ago from now, so tests can build
// deterministic retention orderings without sleeping.
func touchArtifact(t *testing.T, baseDir string, revision Revision, now time.Time, age time.Duration) Artifact {
	t.Helper()

	artifact, err := publishDir(baseDir, revision, func(_ string) error { return nil })
	if err != nil {
		t.Fatalf("publishDir(%s) error = %v", revision, err)
	}

	modTime := now.Add(-age)
	if err := os.Chtimes(artifact.Path, modTime, modTime); err != nil {
		t.Fatalf("Chtimes(%s) error = %v", revision, err)
	}

	return artifact
}

func revisionSet(revisions ...Revision) set.Set[Revision] {
	return set.New(revisions...)
}

func containsRevision(artifacts []Artifact, revision Revision) bool {
	for _, a := range artifacts {
		if a.Revision == revision {
			return true
		}
	}

	return false
}

func TestSweep_LiveArtifactNeverRemoved(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	now := time.Now()

	// Old enough to be past both retention records and TTL on its own.
	touchArtifact(t, baseDir, "live", now, 24*time.Hour)

	result, err := Sweep(baseDir, revisionSet("live"), GCOptions{RetentionRecords: 0, RetentionTTL: time.Minute}, now)
	if err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}

	if len(result.Removed) != 0 {
		t.Errorf("Sweep() removed = %v, want none", result.Removed)
	}

	if !containsRevision(result.Kept, "live") {
		t.Errorf("Sweep() kept = %v, want to include %q", result.Kept, "live")
	}
}

func TestSweep_RetentionRecordsKeepsNewestUnreferenced(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	now := time.Now()

	// All older than the TTL, so only RetentionRecords protects the newest.
	touchArtifact(t, baseDir, "oldest", now, 3*time.Hour)
	touchArtifact(t, baseDir, "middle", now, 2*time.Hour)
	touchArtifact(t, baseDir, "newest", now, 1*time.Hour)

	result, err := Sweep(baseDir, nil, GCOptions{RetentionRecords: 1, RetentionTTL: time.Minute}, now)
	if err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}

	if !containsRevision(result.Kept, "newest") {
		t.Errorf("Sweep() kept = %v, want to include %q", result.Kept, "newest")
	}

	for _, rev := range []Revision{"oldest", "middle"} {
		if !containsRevision(result.Removed, rev) {
			t.Errorf("Sweep() removed = %v, want to include %q", result.Removed, rev)
		}
	}
}

func TestSweep_RetentionTTLKeepsRecentUnreferenced(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	now := time.Now()

	touchArtifact(t, baseDir, "recent", now, time.Second)

	result, err := Sweep(baseDir, nil, GCOptions{RetentionRecords: 0, RetentionTTL: time.Hour}, now)
	if err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}

	if len(result.Removed) != 0 {
		t.Errorf("Sweep() removed = %v, want none (within TTL)", result.Removed)
	}

	if !containsRevision(result.Kept, "recent") {
		t.Errorf("Sweep() kept = %v, want to include %q", result.Kept, "recent")
	}
}

func TestSweep_RemovesUnreferencedExpiredArtifact(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	now := time.Now()

	touchArtifact(t, baseDir, "expired", now, 2*time.Hour)

	result, err := Sweep(baseDir, nil, GCOptions{RetentionRecords: 0, RetentionTTL: time.Minute}, now)
	if err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}

	if !containsRevision(result.Removed, "expired") {
		t.Fatalf("Sweep() removed = %v, want to include %q", result.Removed, "expired")
	}

	if _, err := os.Stat(filepath.Join(baseDir, ArtifactsSubdir, "expired")); !os.IsNotExist(err) {
		t.Errorf("expired artifact directory still exists on disk, stat err = %v", err)
	}
}

func TestSweep_NeverTouchesMirrorOrSubmodulesOrTemp(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	now := time.Now()

	// Sibling state a real GitStore keeps alongside "artifacts/".
	for _, dir := range []string{MirrorSubdir, "submodules"} {
		if err := os.MkdirAll(filepath.Join(baseDir, dir), 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	if err := os.WriteFile(filepath.Join(baseDir, "mirror.lock"), []byte(""), 0o600); err != nil {
		t.Fatalf("create mirror.lock: %v", err)
	}

	touchArtifact(t, baseDir, "expired", now, 2*time.Hour)

	// An in-progress publish must never be considered, let alone removed.
	tmpDir := filepath.Join(baseDir, ArtifactsSubdir, tempArtifactPrefix+"inflight")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatalf("create temp artifact dir: %v", err)
	}

	if _, err := Sweep(baseDir, nil, GCOptions{RetentionRecords: 0, RetentionTTL: time.Minute}, now); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}

	for _, path := range []string{
		filepath.Join(baseDir, MirrorSubdir),
		filepath.Join(baseDir, "submodules"),
		filepath.Join(baseDir, "mirror.lock"),
		tmpDir,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("Sweep() removed or touched %s unexpectedly: stat err = %v", path, err)
		}
	}
}

func TestSweep_NoArtifactsDir_ReturnsEmptyResult(t *testing.T) {
	t.Parallel()

	result, err := Sweep(t.TempDir(), nil, GCOptions{RetentionRecords: 2, RetentionTTL: time.Minute}, time.Now())
	if err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}

	if len(result.Kept) != 0 || len(result.Removed) != 0 {
		t.Errorf("Sweep() = %+v, want empty result", result)
	}
}
