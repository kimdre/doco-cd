package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

const (
	ArtifactMissing  = "missing"
	ArtifactReplaced = "replaced"
)

// ArtifactStaleReason reports whether tasks deployed at deployedAt lost their artifact directory.
func ArtifactStaleReason(dir, deployedAt string) (string, error) {
	return artifactStaleReason(dir, deployedAt, filesystem.BirthTime)
}

func artifactStaleReason(dir, deployedAt string, birthTime func(string) (time.Time, bool, error)) (string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ArtifactMissing, nil
		}

		return "", err
	}

	if !info.IsDir() {
		return ArtifactMissing, nil
	}

	deployed, err := time.Parse(time.RFC3339, strings.TrimSpace(deployedAt))
	if err != nil {
		return "", nil //nolint:nilerr // Legacy deployments may not have a deployment time.
	}

	recorded, err := os.ReadFile(dir + publicationTimeSuffix)
	if err == nil {
		replaced, err := time.Parse(time.RFC3339Nano, string(recorded))
		if err != nil {
			return "", fmt.Errorf("parse artifact publication time: %w", err)
		}

		if replaced.After(deployed) {
			return ArtifactReplaced, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	created, ok, err := birthTime(dir)
	if err != nil {
		return "", err
	}

	if !ok {
		return "", nil
	}

	// Legacy deployment labels have a precision of one second.
	if created.After(deployed.Add(time.Second)) {
		return ArtifactReplaced, nil
	}

	return "", nil
}
