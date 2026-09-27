package filesystem

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func readTestFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(content)
}

func TestSyncInPlace_CopiesTree(t *testing.T) {
	t.Parallel()

	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")

	writeTestTree(t, src, map[string]string{"a.txt": "a", "sub/b.txt": "b", "empty/": ""})

	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	entries, changed, err := SyncInPlace(src, dst)
	if err != nil {
		t.Fatal(err)
	}

	wantEntries := []string{".", "a.txt", "empty", "link", "sub", "sub/b.txt"}
	if !slices.Equal(entries, wantEntries) {
		t.Fatalf("entries = %v, want %v", entries, wantEntries)
	}

	if len(changed) != len(wantEntries) {
		t.Fatalf("changed = %v, want all entries", changed)
	}

	equal, err := ContentEqual(src, dst)
	if err != nil {
		t.Fatal(err)
	}

	if !equal {
		t.Fatal("expected synced tree to equal its source")
	}

	_, changed, err = SyncInPlace(src, dst)
	if err != nil {
		t.Fatal(err)
	}

	if len(changed) != 0 {
		t.Fatalf("changed = %v on unchanged source, want none", changed)
	}
}

func TestSyncInPlace_KeepsInodeOfModifiedFiles(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dst := t.TempDir()

	writeTestTree(t, src, map[string]string{"app.conf": "v1"})
	writeTestTree(t, dst, map[string]string{"app.conf": "old content"})

	before, err := os.Stat(filepath.Join(dst, "app.conf"))
	if err != nil {
		t.Fatal(err)
	}

	_, changed, err := SyncInPlace(filepath.Join(src, "app.conf"), filepath.Join(dst, "app.conf"))
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(changed, []string{"."}) {
		t.Fatalf("changed = %v, want [.]", changed)
	}

	after, err := os.Stat(filepath.Join(dst, "app.conf"))
	if err != nil {
		t.Fatal(err)
	}

	if !os.SameFile(before, after) {
		t.Fatal("expected the modified file to keep its inode")
	}

	if got := readTestFile(t, filepath.Join(dst, "app.conf")); got != "v1" {
		t.Fatalf("content = %q, want v1", got)
	}
}

func TestSyncInPlace_SyncsModes(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dst := t.TempDir()

	writeTestTree(t, src, map[string]string{"dir/script.sh": "echo", "dir/readonly": "new"})
	writeTestTree(t, dst, map[string]string{"dir/script.sh": "echo", "dir/readonly": "old"})

	if err := os.Chmod(filepath.Join(src, "dir", "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(filepath.Join(dst, "dir", "readonly"), 0o444); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(filepath.Join(src, "dir"), 0o555); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(filepath.Join(dst, "dir"), 0o555); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(src, "dir"), PermDir)
		_ = os.Chmod(filepath.Join(dst, "dir"), PermDir)
	})

	_, changed, err := SyncInPlace(filepath.Join(src, "dir"), filepath.Join(dst, "dir"))
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"readonly", "script.sh"}; !slices.Equal(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}

	equal, err := ContentEqual(filepath.Join(src, "dir"), filepath.Join(dst, "dir"))
	if err != nil {
		t.Fatal(err)
	}

	if !equal {
		t.Fatal("expected synced tree to equal its source")
	}
}

func TestSyncInPlace_ReplacesTypeConflictsAndSymlinks(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dst := t.TempDir()

	writeTestTree(t, src, map[string]string{"entry/file": "f", "file": "content"})
	writeTestTree(t, dst, map[string]string{"entry": "was a file", "file/nested": "was a dir"})

	if err := os.Symlink("new-target", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("old-target", filepath.Join(dst, "link")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := SyncInPlace(src, dst); err != nil {
		t.Fatal(err)
	}

	equal, err := ContentEqual(src, dst)
	if err != nil {
		t.Fatal(err)
	}

	if !equal {
		t.Fatal("expected synced tree to equal its source")
	}
}

func TestSyncInPlace_KeepsExtraEntries(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dst := t.TempDir()

	writeTestTree(t, src, map[string]string{"a.txt": "a"})
	writeTestTree(t, dst, map[string]string{"a.txt": "a", "extra/runtime.db": "data"})

	entries, changed, err := SyncInPlace(src, dst)
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{".", "a.txt"}; !slices.Equal(entries, want) {
		t.Fatalf("entries = %v, want %v", entries, want)
	}

	if len(changed) != 0 {
		t.Fatalf("changed = %v, want none", changed)
	}

	if got := readTestFile(t, filepath.Join(dst, "extra", "runtime.db")); got != "data" {
		t.Fatalf("extra content = %q, want data", got)
	}
}

func TestSyncInPlace_MissingSource(t *testing.T) {
	t.Parallel()

	dst := t.TempDir()
	writeTestTree(t, dst, map[string]string{"a.txt": "a"})

	entries, changed, err := SyncInPlace(filepath.Join(t.TempDir(), "missing"), dst)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 || len(changed) != 0 {
		t.Fatalf("entries = %v, changed = %v, want none", entries, changed)
	}

	if got := readTestFile(t, filepath.Join(dst, "a.txt")); got != "a" {
		t.Fatalf("content = %q, want a", got)
	}
}
