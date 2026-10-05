package stages

import (
	"log/slog"
	"slices"
	"strings"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/source/store"
)

const (
	artifactMissing  = store.ArtifactMissing
	artifactReplaced = store.ArtifactReplaced
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
// must be recreated even if nothing changed. Republication is recorded durably by the store, independently of
// filesystem birth-time support. Birth times also detect older, unrecorded replacements where supported.
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
	return docker.DeployedArtifactDirs(labels, dataMountSource, dataMountDestination)
}

// dataMountPath translates a path on the Docker host below the data mount to the path in this container.
func dataMountPath(hostPath, dataMountSource, dataMountDestination string) (string, bool) {
	return docker.DataMountPath(hostPath, dataMountSource, dataMountDestination)
}

// artifactStoreBase returns the store directory of the artifact of revision that path is in.
func artifactStoreBase(path string, revision store.Revision) (string, bool) {
	return docker.ArtifactStoreBase(path, revision)
}

// artifactStaleReason reports why the containers of a service deployed at deployedAt no longer see the artifact
// directory dir, or an empty string if they still do (or this cannot be told).
func artifactStaleReason(dir, deployedAt string) (string, error) {
	return store.ArtifactStaleReason(dir, deployedAt)
}
