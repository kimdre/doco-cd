package cache

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

func TestRevisionRoundTrip(t *testing.T) {
	dataMountPath := t.TempDir()
	sourcePath := filepath.Join(dataMountPath, "registry.example.com", "owner", "artifact")

	if err := WriteRevision(dataMountPath, sourcePath, config.SourceTypeOCI, "sha256:first"); err != nil {
		t.Fatalf("WriteRevision() error = %v", err)
	}

	if err := WriteRevision(dataMountPath, sourcePath, config.SourceTypeOCI, "sha256:second"); err != nil {
		t.Fatalf("WriteRevision() replacement error = %v", err)
	}

	revision, err := ReadRevision(dataMountPath, sourcePath, config.SourceTypeOCI)
	if err != nil {
		t.Fatalf("ReadRevision() error = %v", err)
	}

	if revision != "sha256:second" {
		t.Fatalf("ReadRevision() = %q, want %q", revision, "sha256:second")
	}

	if err := RemoveRevision(dataMountPath, sourcePath, config.SourceTypeOCI); err != nil {
		t.Fatalf("RemoveRevision() error = %v", err)
	}

	if _, err := ReadRevision(dataMountPath, sourcePath, config.SourceTypeOCI); err == nil {
		t.Fatal("ReadRevision() succeeded after RemoveRevision()")
	}
}

func TestRevisionRejectsSourceOutsideDataMount(t *testing.T) {
	dataMountPath := t.TempDir()

	err := WriteRevision(dataMountPath, filepath.Join(filepath.Dir(dataMountPath), "outside"), config.SourceTypeOCI, "sha256:test")
	if !errors.Is(err, filesystem.ErrPathTraversal) {
		t.Fatalf("WriteRevision() error = %v, want ErrPathTraversal", err)
	}
}
