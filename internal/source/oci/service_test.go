package oci

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

func TestValidateDocoLayoutV1_AcceptsVersionInConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.yaml"), []byte("version: doco.v1\nname: app\n"), filesystem.PermOwner); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := validateDocoLayoutV1(dir, ""); err != nil {
		t.Fatalf("validate layout: %v", err)
	}
}

func TestValidateDocoLayoutV1_AcceptsMissingVersion_DefaultsToV1(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.yaml"), []byte("name: app\n"), filesystem.PermOwner); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := validateDocoLayoutV1(dir, ""); err != nil {
		t.Fatalf("validate layout: %v", err)
	}
}

func TestValidateDocoLayoutV1_RejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.yaml"), []byte("version: doco.v2\nname: app\n"), filesystem.PermOwner); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := validateDocoLayoutV1(dir, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestValidateDocoLayoutV1_AcceptsCustomTargetConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.production.yaml"), []byte("version: doco.v1\nname: app\n"), filesystem.PermOwner); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := validateDocoLayoutV1(dir, "production"); err != nil {
		t.Fatalf("validate layout: %v", err)
	}
}

func TestValidateDocoLayoutV1_CustomTargetDoesNotFallBackToDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Only the default config exists — custom target must NOT fall back to it.
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.yaml"), []byte("version: doco.v1\nname: app\n"), filesystem.PermOwner); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := validateDocoLayoutV1(dir, "production")
	if err == nil {
		t.Fatal("expected error when only default config exists and a custom target is set, got nil")
	}
}

func TestValidateDocoLayoutV1_CustomTargetMissingBothFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := validateDocoLayoutV1(dir, "staging")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestFindArtifactConfigFile_NoCustomTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.yml"), []byte("name: app\n"), filesystem.PermOwner); err != nil {
		t.Fatalf("write config: %v", err)
	}

	got, err := findArtifactConfigFile(dir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if filepath.Base(got) != ".doco-cd.yml" {
		t.Fatalf("expected .doco-cd.yml, got %s", filepath.Base(got))
	}
}

func TestFindArtifactConfigFile_CustomTargetDoesNotFallBackToDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Only the default config exists — should not be returned when a custom target is set.
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.yaml"), []byte("name: app\n"), filesystem.PermOwner); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := findArtifactConfigFile(dir, "production")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestFindArtifactConfigFile_CustomTargetReturnsTargetConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for _, name := range []string{".doco-cd.yaml", ".doco-cd.production.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("name: app\n"), filesystem.PermOwner); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	got, err := findArtifactConfigFile(dir, "production")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if filepath.Base(got) != ".doco-cd.production.yaml" {
		t.Fatalf("expected .doco-cd.production.yaml, got %s", filepath.Base(got))
	}
}

func TestSyncDirectoryContents_PreservesDestinationInode(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	dst := filepath.Join(base, "destination")

	if err := os.MkdirAll(dst, filesystem.PermDir); err != nil {
		t.Fatalf("create dst: %v", err)
	}

	// Record the inode of the destination directory before the sync.
	inodeBefore := dirInode(t, dst)

	// Populate a source directory.
	src := filepath.Join(base, "source")
	if err := os.MkdirAll(src, filesystem.PermDir); err != nil {
		t.Fatalf("create src: %v", err)
	}

	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("hello"), filesystem.PermPublic); err != nil {
		t.Fatalf("write src file: %v", err)
	}

	if err := syncDirectoryContents(src, dst); err != nil {
		t.Fatalf("syncDirectoryContents: %v", err)
	}

	// The destination directory inode must be unchanged.
	inodeAfter := dirInode(t, dst)
	if inodeBefore != inodeAfter {
		t.Fatalf("destination inode changed: before=%d after=%d — bind mounts would be broken", inodeBefore, inodeAfter)
	}

	// The new file must be present.
	if _, err := os.Stat(filepath.Join(dst, "file.txt")); err != nil {
		t.Fatalf("expected file.txt in destination: %v", err)
	}
}

func TestSyncDirectoryContents_RemovesStaleEntries(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	dst := filepath.Join(base, "destination")

	if err := os.MkdirAll(dst, filesystem.PermDir); err != nil {
		t.Fatalf("create dst: %v", err)
	}

	// Write a stale file that must be removed during sync.
	stale := filepath.Join(dst, "stale.txt")
	if err := os.WriteFile(stale, []byte("old"), filesystem.PermPublic); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	src := filepath.Join(base, "source")
	if err := os.MkdirAll(src, filesystem.PermDir); err != nil {
		t.Fatalf("create src: %v", err)
	}

	if err := os.WriteFile(filepath.Join(src, "new.txt"), []byte("new"), filesystem.PermPublic); err != nil {
		t.Fatalf("write src file: %v", err)
	}

	if err := syncDirectoryContents(src, dst); err != nil {
		t.Fatalf("syncDirectoryContents: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale.txt should have been removed from destination")
	}

	if _, err := os.Stat(filepath.Join(dst, "new.txt")); err != nil {
		t.Fatalf("new.txt should exist in destination: %v", err)
	}
}

// dirInode returns the inode number of a directory, failing the test on error.
func dirInode(t *testing.T, path string) uint64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("cannot read inode on this platform")
	}

	return sys.Ino
}
