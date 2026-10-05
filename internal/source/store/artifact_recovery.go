package store

import (
	"errors"
	"io/fs"
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
	created, ok, err := filesystem.BirthTime(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ArtifactMissing, nil
		}

		return "", err
	}

	if !filesystem.IsDir(dir) {
		return ArtifactMissing, nil
	}

	if !ok {
		return "", nil
	}

	deployed, err := time.Parse(time.RFC3339, strings.TrimSpace(deployedAt))
	if err != nil {
		return "", nil //nolint:nilerr // Legacy deployments may not have a deployment time.
	}

	// Deployment labels have a precision of one second.
	if created.After(deployed.Add(time.Second)) {
		return ArtifactReplaced, nil
	}

	return "", nil
}
