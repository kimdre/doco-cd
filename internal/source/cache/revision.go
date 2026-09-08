package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

const revisionDirectory = ".doco-cd/source-revisions"

// RemoveRevision invalidates the recorded revision before cached source contents are replaced.
func RemoveRevision(dataMountPath, sourcePath string, sourceType config.SourceType) error {
	markerPath, err := revisionMarkerPath(dataMountPath, sourcePath, sourceType)
	if err != nil {
		return err
	}

	if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove source revision marker: %w", err)
	}

	return nil
}

// WriteRevision atomically records the immutable revision represented by a cached source directory.
func WriteRevision(dataMountPath, sourcePath string, sourceType config.SourceType, revision string) error {
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return errors.New("source revision is empty")
	}

	markerPath, err := revisionMarkerPath(dataMountPath, sourcePath, sourceType)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(markerPath), filesystem.PermDir); err != nil {
		return fmt.Errorf("create source revision directory: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(markerPath), ".revision-*")
	if err != nil {
		return fmt.Errorf("create temporary source revision marker: %w", err)
	}

	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.WriteString(revision + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write source revision marker: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync source revision marker: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close source revision marker: %w", err)
	}

	if err := os.Rename(tmpPath, markerPath); err != nil {
		return fmt.Errorf("replace source revision marker: %w", err)
	}

	return nil
}

// ReadRevision returns the immutable revision recorded for a cached source directory.
func ReadRevision(dataMountPath, sourcePath string, sourceType config.SourceType) (string, error) {
	markerPath, err := revisionMarkerPath(dataMountPath, sourcePath, sourceType)
	if err != nil {
		return "", err
	}

	content, err := os.ReadFile(markerPath)
	if err != nil {
		return "", fmt.Errorf("read source revision marker: %w", err)
	}

	revision := strings.TrimSpace(string(content))
	if revision == "" {
		return "", errors.New("source revision marker is empty")
	}

	return revision, nil
}

func revisionMarkerPath(dataMountPath, sourcePath string, sourceType config.SourceType) (string, error) {
	sourceType = config.NormalizeSourceType(sourceType)
	if err := config.ValidateSourceType(sourceType); err != nil {
		return "", err
	}

	dataMountPath, err := filepath.Abs(dataMountPath)
	if err != nil {
		return "", fmt.Errorf("resolve data mount path: %w", err)
	}

	sourcePath, err = filepath.Abs(sourcePath)
	if err != nil {
		return "", fmt.Errorf("resolve source path: %w", err)
	}

	relativePath, err := filepath.Rel(dataMountPath, sourcePath)
	if err != nil {
		return "", fmt.Errorf("resolve source path relative to data mount: %w", err)
	}

	if relativePath == "." || relativePath == ".." ||
		filepath.IsAbs(relativePath) ||
		strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: source path %s is outside data mount %s",
			filesystem.ErrPathTraversal, sourcePath, dataMountPath)
	}

	key := sha256.Sum256([]byte(string(sourceType) + "\x00" + filepath.ToSlash(relativePath)))
	filename := hex.EncodeToString(key[:]) + ".revision"

	return filepath.Join(dataMountPath, revisionDirectory, filename), nil
}
