package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

func TestPublishDir_CreatesReadableArtifact(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()

	artifact, err := publishDir(baseDir, "rev1", func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content"), 0o600)
	})
	if err != nil {
		t.Fatalf("publishDir() error = %v", err)
	}

	wantPath := artifactPath(baseDir, "rev1")
	if artifact.Path != wantPath {
		t.Fatalf("artifact.Path = %q, want %q", artifact.Path, wantPath)
	}

	info, err := os.Stat(artifact.Path)
	if err != nil {
		t.Fatalf("stat artifact path: %v", err)
	}

	// os.MkdirTemp always creates its directory 0700; publishDir must widen
	// it back to filesystem.PermDir so the artifact - bind mounted directly
	// into deployed containers - stays readable by non-owner processes.
	if info.Mode().Perm() != filesystem.PermDir {
		t.Errorf("artifact dir mode = %v, want %v", info.Mode().Perm(), os.FileMode(filesystem.PermDir))
	}

	content, err := os.ReadFile(filepath.Join(artifact.Path, "file.txt"))
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}

	if string(content) != "content" {
		t.Errorf("published file content = %q, want %q", content, "content")
	}
}

func TestPublishDir_WriteFailure_LeavesNoArtifactAndCleansUpTemp(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	wantErr := errors.New("simulated crash during export")

	_, err := publishDir(baseDir, "rev1", func(dir string) error {
		// Simulate a crash partway through writing the artifact: some
		// content is written before the failure.
		_ = os.WriteFile(filepath.Join(dir, "partial.txt"), []byte("partial"), 0o600)
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("publishDir() error = %v, want %v", err, wantErr)
	}

	if _, found, lookupErr := lookupArtifact(baseDir, "rev1"); lookupErr != nil || found {
		t.Fatalf("lookupArtifact() = (found=%v, err=%v), want (found=false, err=nil)", found, lookupErr)
	}

	entries, err := os.ReadDir(filepath.Join(baseDir, ArtifactsSubdir))
	if err != nil {
		t.Fatalf("read artifacts dir: %v", err)
	}

	for _, e := range entries {
		t.Errorf("unexpected leftover entry after failed publish: %s", e.Name())
	}
}

func TestPublishDir_ConcurrentDifferentRevisions_BothSucceedIndependently(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()

	revisions := []Revision{"rev1", "rev2", "rev3"}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for _, rev := range revisions {
		wg.Add(1)

		go func(rev Revision) {
			defer wg.Done()

			_, err := publishDir(baseDir, rev, func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "marker.txt"), []byte(rev), 0o600)
			})

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errs = append(errs, err)
			}
		}(rev)
	}

	wg.Wait()

	for _, err := range errs {
		t.Errorf("publishDir() error = %v", err)
	}

	for _, rev := range revisions {
		content, err := os.ReadFile(filepath.Join(artifactPath(baseDir, rev), "marker.txt"))
		if err != nil {
			t.Fatalf("read published file for %s: %v", rev, err)
		}

		if string(content) != string(rev) {
			t.Errorf("revision %s content = %q, want %q (revisions must not interfere with each other)", rev, content, rev)
		}
	}
}

func TestPublishDir_ConcurrentSameRevision_IsIdempotent(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()

	const attempts = 8

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []Artifact
		errs    []error
	)

	for range attempts {
		wg.Go(func() {
			artifact, err := publishDir(baseDir, "rev1", func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("content"), 0o600)
			})

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errs = append(errs, err)
				return
			}

			results = append(results, artifact)
		})
	}

	wg.Wait()

	for _, err := range errs {
		t.Errorf("publishDir() error = %v", err)
	}

	if len(results) != attempts {
		t.Fatalf("got %d successful results, want %d", len(results), attempts)
	}

	want := artifactPath(baseDir, "rev1")
	for _, artifact := range results {
		if artifact.Path != want {
			t.Errorf("artifact.Path = %q, want %q (every concurrent publish of the same revision must agree on one path)", artifact.Path, want)
		}
	}

	// Exactly one published artifact directory exists for the revision -
	// concurrent losers must not each leave their own copy behind.
	entries, err := os.ReadDir(filepath.Join(baseDir, ArtifactsSubdir))
	if err != nil {
		t.Fatalf("read artifacts dir: %v", err)
	}

	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}

		t.Fatalf("artifacts dir entries = %v, want exactly one", names)
	}
}

func TestSweepOrphanedTemp_RemovesOnlyTmpDirs(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()

	if _, err := publishDir(baseDir, "rev1", func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content"), 0o600)
	}); err != nil {
		t.Fatalf("publishDir() error = %v", err)
	}

	artifactsDir := filepath.Join(baseDir, ArtifactsSubdir)

	orphan := filepath.Join(artifactsDir, ".tmp-orphaned-from-a-crash")
	if err := os.MkdirAll(orphan, filesystem.PermDir); err != nil {
		t.Fatalf("create orphaned temp dir: %v", err)
	}

	aged := time.Now().Add(-2 * orphanedTempMaxAge)
	if err := os.Chtimes(orphan, aged, aged); err != nil {
		t.Fatalf("age orphaned temp dir: %v", err)
	}

	if err := sweepOrphanedTemp(baseDir); err != nil {
		t.Fatalf("sweepOrphanedTemp() error = %v", err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("expected orphaned temp dir to be removed, stat err = %v", err)
	}

	if _, found, err := lookupArtifact(baseDir, "rev1"); err != nil || !found {
		t.Errorf("expected legitimate artifact to survive sweep, found=%v err=%v", found, err)
	}
}

// A store is constructed per deployment, so a sweep must never destroy a
// temporary directory another store is still publishing into.
func TestSweepOrphanedTemp_KeepsRecentTmpDirs(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()

	artifactsDir := filepath.Join(baseDir, ArtifactsSubdir)
	if err := os.MkdirAll(artifactsDir, filesystem.PermDir); err != nil {
		t.Fatalf("create artifacts dir: %v", err)
	}

	inFlight := filepath.Join(artifactsDir, ".tmp-still-being-written")
	if err := os.MkdirAll(inFlight, filesystem.PermDir); err != nil {
		t.Fatalf("create in-flight temp dir: %v", err)
	}

	if err := sweepOrphanedTemp(baseDir); err != nil {
		t.Fatalf("sweepOrphanedTemp() error = %v", err)
	}

	if _, err := os.Stat(inFlight); err != nil {
		t.Errorf("expected in-flight temp dir to survive sweep, stat err = %v", err)
	}
}

func TestSweepOrphanedTemp_NoArtifactsDir_NoError(t *testing.T) {
	t.Parallel()

	if err := sweepOrphanedTemp(t.TempDir()); err != nil {
		t.Errorf("sweepOrphanedTemp() error = %v, want nil", err)
	}
}

func TestLookupArtifact_NotFoundVsFound(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()

	if _, found, err := lookupArtifact(baseDir, "rev1"); err != nil || found {
		t.Fatalf("lookupArtifact() before publish = (found=%v, err=%v), want (false, nil)", found, err)
	}

	if _, err := publishDir(baseDir, "rev1", func(_ string) error {
		return nil
	}); err != nil {
		t.Fatalf("publishDir() error = %v", err)
	}

	artifact, found, err := lookupArtifact(baseDir, "rev1")
	if err != nil || !found {
		t.Fatalf("lookupArtifact() after publish = (found=%v, err=%v), want (true, nil)", found, err)
	}

	if artifact.Revision != "rev1" {
		t.Errorf("artifact.Revision = %q, want %q", artifact.Revision, "rev1")
	}
}

func TestListArtifacts_SkipsNonDirectoryEntries(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()

	for _, rev := range []Revision{"rev1", "rev2"} {
		if _, err := publishDir(baseDir, rev, func(_ string) error { return nil }); err != nil {
			t.Fatalf("publishDir(%s) error = %v", rev, err)
		}
	}

	// A stray non-directory entry (e.g. left over from manual inspection)
	// must never be reported as a published artifact.
	strayFile := filepath.Join(baseDir, ArtifactsSubdir, "not-a-revision.txt")
	if err := os.WriteFile(strayFile, []byte("stray"), 0o600); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	artifacts, err := listArtifacts(baseDir)
	if err != nil {
		t.Fatalf("listArtifacts() error = %v", err)
	}

	if len(artifacts) != 2 {
		t.Fatalf("listArtifacts() returned %d artifacts, want 2", len(artifacts))
	}

	seen := set.New[Revision]()
	for _, a := range artifacts {
		seen.Add(a.Revision)
	}

	for _, rev := range []Revision{"rev1", "rev2"} {
		if !seen.Contains(rev) {
			t.Errorf("listArtifacts() missing revision %s", rev)
		}
	}
}

func TestListArtifacts_SkipsTempArtifactDirectories(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()

	if _, err := publishDir(baseDir, "rev1", func(_ string) error { return nil }); err != nil {
		t.Fatalf("publishDir() error = %v", err)
	}

	// A temporary directory left by a concurrent, still-in-progress
	// publishDir call must never be reported as a published artifact -
	// otherwise callers built on List() (e.g. garbage collection) could
	// observe and act on another caller's in-flight publish.
	artifactsDir := filepath.Join(baseDir, ArtifactsSubdir)
	if err := os.Mkdir(filepath.Join(artifactsDir, tempArtifactPrefix+"abc123"), 0o755); err != nil {
		t.Fatalf("create temp artifact dir: %v", err)
	}

	artifacts, err := listArtifacts(baseDir)
	if err != nil {
		t.Fatalf("listArtifacts() error = %v", err)
	}

	if len(artifacts) != 1 {
		t.Fatalf("listArtifacts() returned %d artifacts, want 1: %+v", len(artifacts), artifacts)
	}

	if artifacts[0].Revision != "rev1" {
		t.Errorf("listArtifacts()[0].Revision = %q, want %q", artifacts[0].Revision, "rev1")
	}
}

func TestListArtifacts_NoArtifactsDir_ReturnsEmpty(t *testing.T) {
	t.Parallel()

	artifacts, err := listArtifacts(t.TempDir())
	if err != nil {
		t.Fatalf("listArtifacts() error = %v", err)
	}

	if len(artifacts) != 0 {
		t.Errorf("listArtifacts() = %v, want empty", artifacts)
	}
}

func TestArtifactDirNameRoundTrip(t *testing.T) {
	for _, revision := range []Revision{
		"5dca9e2cb6c87863a3d3ff8e72d915bdf101e8ec2d6a30b1e4563a53864e07ad",
		"revision-with-hyphens",
		"sha256:5dca9e2cb6c87863a3d3ff8e72d915bdf101e8ec2d6a30b1e4563a53864e07ad",
		"sha512:abc",
	} {
		name := artifactDirName(revision)
		if strings.Contains(name, ":") {
			t.Errorf("artifactDirName(%q) = %q, must not contain a colon", revision, name)
		}

		if got := revisionFromDirName(name); got != revision {
			t.Errorf("revisionFromDirName(%q) = %q, want %q", name, got, revision)
		}
	}
}
