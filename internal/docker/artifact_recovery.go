package docker

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/moby/moby/client"

	swarmInternal "github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// DeployedArtifactDirs translates a service's mounted artifact paths into the data mount.
func DeployedArtifactDirs(labels Labels, dataMountSource, dataMountDestination string) []string {
	revision := store.Revision(strings.TrimSpace(labels[DocoCDLabels.Deployment.CommitSHA]))
	if revision == "" {
		return nil
	}

	workingDir, ok := DataMountPath(labels[DocoCDLabels.Deployment.WorkingDir], dataMountSource, dataMountDestination)
	if !ok {
		return nil
	}

	storeBase, ok := ArtifactStoreBase(workingDir, revision)
	if !ok {
		return nil
	}

	revisions := []store.Revision{revision}
	if pinned := ParsePinnedRevisions(labels[DocoCDLabels.Deployment.PinnedRevisions]); len(pinned) > 0 {
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

// DataMountPath translates a Docker host path below the data mount into this container.
func DataMountPath(hostPath, dataMountSource, dataMountDestination string) (string, bool) {
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

// ArtifactStoreBase returns the store of revision that path is nested under.
func ArtifactStoreBase(path string, revision store.Revision) (string, bool) {
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

// staleSwarmArtifactServices checks existing task generations before a rotation updates service metadata.
func staleSwarmArtifactServices(ctx context.Context, apiClient client.APIClient, namespace string, load ComposeLoadOptions) ([]string, error) {
	services, err := swarmInternal.GetStackServices(ctx, apiClient, namespace)
	if err != nil {
		return nil, err
	}

	var stale []string

	for _, service := range services {
		if service.Spec.Mode.ReplicatedJob != nil || service.Spec.Mode.GlobalJob != nil {
			continue
		}

		labels := SwarmServiceLabels(service)
		for _, dir := range DeployedArtifactDirs(labels, load.DataHostPath, load.DataMountPath) {
			reason, err := store.ArtifactStaleReason(dir, labels[DocoCDLabels.Deployment.Timestamp])
			if err != nil {
				return nil, fmt.Errorf("check artifact of service %s: %w", service.Spec.Name, err)
			}

			if reason != "" {
				stale = append(stale, strings.TrimPrefix(service.Spec.Name, namespace+"_"))
				break
			}
		}
	}

	slices.Sort(stale)

	return stale, nil
}
