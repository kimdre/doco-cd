package filesystem

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBirthTime(t *testing.T) {
	t.Parallel()

	before := time.Now().Add(-time.Second)
	dir := t.TempDir()

	created, ok, err := BirthTime(dir)
	if err != nil {
		t.Fatalf("BirthTime() error = %v", err)
	}

	if !ok {
		t.Skip("file system does not record creation times")
	}

	if created.Before(before) || created.After(time.Now().Add(time.Second)) {
		t.Fatalf("BirthTime() = %v, want around %v", created, before)
	}
}

func TestBirthTime_Missing(t *testing.T) {
	t.Parallel()

	_, _, err := BirthTime(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("BirthTime() error = %v, want %v", err, fs.ErrNotExist)
	}
}

func TestIdentity(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	dir := filepath.Join(base, "dir")

	if err := os.Mkdir(dir, PermDir); err != nil {
		t.Fatal(err)
	}

	original, err := Identity(dir)
	if err != nil {
		t.Fatalf("Identity() error = %v", err)
	}

	if original == "" {
		t.Skip("platform does not expose file identities")
	}

	renamed := filepath.Join(base, "renamed")
	if err = os.Rename(dir, renamed); err != nil {
		t.Fatal(err)
	}

	if got, err := Identity(renamed); err != nil || got != original {
		t.Fatalf("Identity() after rename = %q, %v, want %q", got, err, original)
	}

	// Keep the original inode allocated, so the re-created directory cannot reuse its inode number.
	if err = os.Mkdir(dir, PermDir); err != nil {
		t.Fatal(err)
	}

	if got, err := Identity(dir); err != nil || got == original {
		t.Fatalf("Identity() of re-created directory = %q, %v, want it to differ from %q", got, err, original)
	}
}

func TestIdentity_Missing(t *testing.T) {
	t.Parallel()

	_, err := Identity(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Identity() error = %v, want %v", err, fs.ErrNotExist)
	}
}
