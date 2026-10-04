package stages

import (
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/source/store"
)

const (
	artifactMissing  = "missing"
	artifactReplaced = "replaced"
)

// staleArtifact is a deployed service whose containers no longer see the artifact directory they were created from.
type staleArtifact struct {
	Service  string
	Artifact string
	Reason   string
}

// staleArtifactServices returns the deployed services whose containers no longer see the content of the artifact
// directory they bind-mount: the artifact they were pinned to, or the one of the revision they were deployed with.
//
// A container keeps a removed directory mounted, so it keeps running with an empty directory, even after the
// artifact was published again under the same path. Artifacts are immutable and only published once, so an artifact
// created after the deployment that created a service's containers was removed in between (for example by
// destroy.remove_dir before it was deprecated, see https://github.com/kimdre/doco-cd/issues/1962). Such services
// must be recreated even if nothing changed. Whether an artifact was published again can only be detected on file
// systems that record the creation time of files; otherwise, only missing artifacts are detected.
func staleArtifactServices(
	deployed map[docker.Service]docker.ServiceStatus, dataMountSource, dataMountDestination string, log *slog.Logger,
) []staleArtifact {
	var stale []staleArtifact

	for service, status := range deployed {
		for _, artifact := range deployedArtifactDirs(status.Labels, dataMountSource, dataMountDestination) {
			reason, err := artifactStaleReason(artifact, status.Labels[docker.DocoCDLabels.Deployment.Timestamp])
			if err != nil {
				log.Debug("failed to check the artifact of a deployed service",
					slog.String("service", string(service)), slog.String("artifact", artifact), logger.ErrAttr(err))

				continue
			}

			if reason != "" {
				stale = append(stale, staleArtifact{Service: string(service), Artifact: artifact, Reason: reason})

				break
			}
		}
	}

	slices.SortFunc(stale, func(a, b staleArtifact) int { return strings.Compare(a.Service, b.Service) })

	return stale
}

// staleArtifactChange returns the change that recreates the services of stale.
func staleArtifactChange(stale []staleArtifact) docker.Change {
	change := docker.Change{Type: docker.ChangeTypeStaleArtifact}
	for _, s := range stale {
		change.Services = append(change.Services, s.Service)
	}

	return change
}

// deployedArtifactDirs returns the paths in this container of the artifact directories a service deployed with
// labels bind-mounts. It returns nothing for services that were not deployed from an artifact below the data mount.
func deployedArtifactDirs(labels docker.Labels, dataMountSource, dataMountDestination string) []string {
	revision := store.Revision(strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.CommitSHA]))
	if revision == "" {
		return nil
	}

	workingDir, ok := dataMountPath(labels[docker.DocoCDLabels.Deployment.WorkingDir], dataMountSource, dataMountDestination)
	if !ok {
		return nil
	}

	storeBase, ok := artifactStoreBase(workingDir, revision)
	if !ok {
		return nil
	}

	// Services whose repository files were unchanged keep mounting the artifact they were pinned to instead.
	revisions := []store.Revision{revision}
	if pinned := docker.ParsePinnedRevisions(labels[docker.DocoCDLabels.Deployment.PinnedRevisions]); len(pinned) > 0 {
		revisions = revisions[:0]
		for _, p := range pinned {
			revisions = append(revisions, store.Revision(p))
		}
	}

	dirs := make([]string, 0, len(revisions))
	for _, r := range revisions {
		dirs = append(dirs, filepath.Join(storeBase, store.ArtifactsSubdir, store.ArtifactDirName(r)))
	}

	return dirs
}

// dataMountPath translates a path on the Docker host below the data mount to the path in this container.
func dataMountPath(hostPath, dataMountSource, dataMountDestination string) (string, bool) {
	hostPath = strings.TrimSpace(hostPath)
	if hostPath == "" || dataMountSource == "" || dataMountDestination == "" || !filepath.IsAbs(hostPath) {
		return "", false
	}

	rel, err := filepath.Rel(dataMountSource, hostPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}

	return filepath.Join(dataMountDestination, rel), true
}

// artifactStoreBase returns the store directory of the artifact of revision that path is in.
func artifactStoreBase(path string, revision store.Revision) (string, bool) {
	dirName := store.ArtifactDirName(revision)

	for dir := filepath.Clean(path); ; {
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}

		if filepath.Base(dir) == dirName && filepath.Base(parent) == store.ArtifactsSubdir {
			return filepath.Dir(parent), true
		}

		dir = parent
	}
}

// artifactStaleReason reports why the containers of a service deployed at deployedAt no longer see the artifact
// directory dir, or an empty string if they still do (or this cannot be told).
func artifactStaleReason(dir, deployedAt string) (string, error) {
	created, ok, err := filesystem.BirthTime(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return artifactMissing, nil
		}

		return "", err
	}

	if !filesystem.IsDir(dir) {
		return artifactMissing, nil
	}

	if !ok {
		return "", nil
	}

	deployed, err := time.Parse(time.RFC3339, strings.TrimSpace(deployedAt))
	if err != nil {
		return "", nil //nolint:nilerr // Without a deployment time, only a missing artifact can be detected.
	}

	// The deployment time is taken after the artifact was published, but only has a precision of one second.
	if created.After(deployed.Add(time.Second)) {
		return artifactReplaced, nil
	}

	return "", nil
}
