package store

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

func mustArtifactPath(t *testing.T, baseDir string, revision Revision) string {
	t.Helper()

	path, err := artifactPath(baseDir, revision)
	if err != nil {
		t.Fatalf("artifactPath(%q) error = %v", revision, err)
	}

	return path
}

func TestArtifactPath_RejectsRevisionsOutsideArtifactsDir(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()

	for _, revision := range []Revision{"", ".", "..", "../rev1", "rev1/../..", "sha256:../../etc", "/abs", `rev\1`, "rev/1"} {
		if path, err := artifactPath(baseDir, revision); !errors.Is(err, ErrInvalidRevision) {
			t.Errorf("artifactPath(%q) = %q, %v, want %v", revision, path, err, ErrInvalidRevision)
		}

		if _, _, err := lookupArtifact(baseDir, revision); !errors.Is(err, ErrInvalidRevision) {
			t.Errorf("lookupArtifact(%q) error = %v, want %v", revision, err, ErrInvalidRevision)
		}

		_, err := publishDir(baseDir, revision, func(string) error {
			t.Errorf("publishDir(%q) wrote an artifact for an invalid revision", revision)
			return nil
		})
		if !errors.Is(err, ErrInvalidRevision) {
			t.Errorf("publishDir(%q) error = %v, want %v", revision, err, ErrInvalidRevision)
		}
	}

	for _, revision := range []Revision{
		"0123456789abcdef0123456789abcdef01234567",
		"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		want := filepath.Join(baseDir, ArtifactsSubdir, artifactDirName(revision))
		if got := mustArtifactPath(t, baseDir, revision); got != want {
			t.Errorf("artifactPath(%q) = %q, want %q", revision, got, want)
		}
	}
}

func TestPublishDir_CreatesReadableArtifact(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()

	artifact, err := publishDir(baseDir, "rev1", func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content"), 0o600)
	})
	if err != nil {
		t.Fatalf("publishDir() error = %v", err)
	}

	wantPath := mustArtifactPath(t, baseDir, "rev1")
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
		content, err := os.ReadFile(filepath.Join(mustArtifactPath(t, baseDir, rev), "marker.txt"))
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

	want := mustArtifactPath(t, baseDir, "rev1")
	for _, artifact := range results {
		if artifact.Path != want {
			t.Errorf("artifact.Path = %q, want %q (every concurrent publish of the same revision must agree on one path)", artifact.Path, want)
		}
	}

	// Exactly one published artifact directory exists for the revision -
	// concurrent losers must not each leave their own copy behind. Next to it
	// are only its publish record and lock.
	entries, err := os.ReadDir(filepath.Join(baseDir, ArtifactsSubdir))
	if err != nil {
		t.Fatalf("read artifacts dir: %v", err)
	}

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}

	if !slices.Equal(names, []string{"rev1", "rev1" + publishLockSuffix + ".lock", "rev1" + publishedSuffix}) {
		t.Fatalf("artifacts dir entries = %v, want the artifact and its publish record and lock", names)
	}
}

func TestSweepOrphanedTemp_RemovesOnlyTmpEntries(t *testing.T) {
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

	orphanedRecord := filepath.Join(artifactsDir, ".tmp-rev2.published-123")
	if err := os.WriteFile(orphanedRecord, nil, 0o600); err != nil {
		t.Fatalf("create orphaned temp record: %v", err)
	}

	aged := time.Now().Add(-2 * orphanedTempMaxAge)
	for _, path := range []string{orphan, orphanedRecord} {
		if err := os.Chtimes(path, aged, aged); err != nil {
			t.Fatalf("age orphaned temp entry: %v", err)
		}
	}

	if err := sweepOrphanedTemp(baseDir); err != nil {
		t.Fatalf("sweepOrphanedTemp() error = %v", err)
	}

	for _, path := range []string{orphan, orphanedRecord} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected orphaned temp entry %s to be removed, stat err = %v", filepath.Base(path), err)
		}
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

func TestPublishDir_RecordsPublishedIdentity(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()

	artifact, err := publishDir(baseDir, "rev1", func(_ string) error { return nil })
	if err != nil {
		t.Fatalf("publishDir() error = %v", err)
	}

	recorded, err := os.ReadFile(artifact.Path + publishedSuffix)
	if err != nil {
		t.Fatalf("read publish record: %v", err)
	}

	identity, err := filesystem.Identity(artifact.Path)
	if err != nil {
		t.Fatalf("Identity() error = %v", err)
	}

	if string(recorded) != identity {
		t.Errorf("publish record = %q, want %q", recorded, identity)
	}
}

// replaceWithSkeleton replaces the artifact directory at path with a new directory tree, as Docker does when it
// re-creates a missing bind-mount source. The original is renamed instead of removed, so the replacement cannot
// reuse its inode.
func replaceWithSkeleton(t *testing.T, path string, files ...string) {
	t.Helper()

	if err := os.Rename(path, path+"-replaced"); err != nil {
		t.Fatalf("move artifact away: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(path, "data", "nested"), filesystem.PermDir); err != nil {
		t.Fatalf("re-create artifact directory: %v", err)
	}

	for _, name := range files {
		if err := os.WriteFile(filepath.Join(path, "data", name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestLookupArtifact_RejectsRecreatedDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("directory identities are only available on Linux")
	}

	t.Parallel()

	for name, files := range map[string][]string{
		"directories only":            nil,
		"written to by the container": {"written-by-container.txt"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			baseDir := t.TempDir()

			artifact, err := publishDir(baseDir, "rev1", func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("v1"), 0o600)
			})
			if err != nil {
				t.Fatalf("publishDir() error = %v", err)
			}

			replaceWithSkeleton(t, artifact.Path, files...)

			if _, found, err := lookupArtifact(baseDir, "rev1"); err != nil || found {
				t.Fatalf("lookupArtifact() of re-created directory = (found=%v, err=%v), want (false, nil)", found, err)
			}

			republished, err := publishDir(baseDir, "rev1", func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("v1"), 0o600)
			})
			if err != nil {
				t.Fatalf("publishDir() republish error = %v", err)
			}

			if content, err := os.ReadFile(filepath.Join(republished.Path, "compose.yaml")); err != nil || string(content) != "v1" {
				t.Errorf("republished compose.yaml = (%q, %v), want (%q, nil)", content, err, "v1")
			}

			if _, found, err := lookupArtifact(baseDir, "rev1"); err != nil || !found {
				t.Errorf("lookupArtifact() after republish = (found=%v, err=%v), want (true, nil)", found, err)
			}

			entries, err := os.ReadDir(filepath.Join(baseDir, ArtifactsSubdir))
			if err != nil {
				t.Fatalf("read artifacts dir: %v", err)
			}

			setAside := false

			for _, e := range entries {
				if e.IsDir() && strings.HasPrefix(e.Name(), tempArtifactPrefix+"unpublished-") {
					setAside = true
				}
			}

			if !setAside {
				t.Errorf("expected the re-created directory to be set aside, artifacts dir entries = %v", entries)
			}
		})
	}
}

func TestLookupArtifact_UnrecordedDirectory(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		file      string
		wantFound bool
	}{
		"published before records adopted": {file: "compose.yaml", wantFound: true},
		"directories only rejected":        {wantFound: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			baseDir := t.TempDir()
			path := mustArtifactPath(t, baseDir, "rev1")

			if err := os.MkdirAll(filepath.Join(path, "data"), filesystem.PermDir); err != nil {
				t.Fatalf("create artifact directory: %v", err)
			}

			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(path, "data", tc.file), []byte("v1"), 0o600); err != nil {
					t.Fatalf("write %s: %v", tc.file, err)
				}
			}

			if _, found, err := lookupArtifact(baseDir, "rev1"); err != nil || found != tc.wantFound {
				t.Fatalf("lookupArtifact() = (found=%v, err=%v), want (%v, nil)", found, err, tc.wantFound)
			}

			_, err := os.Stat(path + publishedSuffix)
			if recorded := err == nil; recorded != tc.wantFound {
				t.Errorf("publish record written = %v (stat err = %v), want %v", recorded, err, tc.wantFound)
			}
		})
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
