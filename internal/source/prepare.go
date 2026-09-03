package source

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/source/oci"
)

// Prepare resolves req's source (Git repository or OCI artifact) into a
// ready-to-deploy local checkout: it normalizes/validates the source type,
// computes safe internal/external filesystem paths, clones/updates the Git
// repository (or resolves/verifies/pulls the OCI artifact), resolves the
// webhook/poll deployment configuration, applies the OCI reference override
// and custom target propagation, and returns the result.
//
// On a Git clone or deploy-configuration resolution failure, Prepare also
// reports the failure as an early commit status (before reconciliation ever
// starts), mirroring the pre-refactor handler's behavior.
func (p *Preparer) Prepare(ctx context.Context, req Request) (result Result, retErr error) {
	startedAt := time.Now()
	sourceLabel := "unknown"

	defer func() {
		outcome := "success"
		if retErr != nil {
			outcome = "failure"
		}

		prometheus.SourcePreparationDuration.WithLabelValues(sourceLabel, outcome).Observe(time.Since(startedAt).Seconds())
	}()

	if err := validation.Validate(req); err != nil {
		return Result{}, wrapPrepareError(ErrInvalidRequest, err)
	}

	sourceType := config.NormalizeSourceType(req.SourceType)
	if err := config.ValidateSourceType(sourceType); err != nil {
		return Result{}, wrapPrepareError(ErrInvalidSourceType, err)
	}

	sourceLabel = string(sourceType)

	repoName := git.GetRepoName(req.SourceRef)
	if sourceType == config.SourceTypeOCI {
		repoName = oci.RepositoryNameFromArtifact(req.SourceRef)
	}

	if strings.Contains(repoName, "..") {
		return Result{}, wrapPrepareError(ErrInvalidRepositoryName, fmt.Errorf("invalid repository name: %s, contains '..'", repoName))
	}

	// Path inside the container.
	internalRepoPath, err := filesystem.VerifyAndSanitizePath(
		filepath.Join(req.DataMountPoint.Destination, repoName),
		req.DataMountPoint.Destination,
	)
	if err != nil {
		return Result{}, wrapPrepareError(ErrInvalidInternalPath, err)
	}

	// Path on the host.
	externalRepoPath, err := filesystem.VerifyAndSanitizePath(
		filepath.Join(req.DataMountPoint.Source, repoName),
		req.DataMountPoint.Source,
	)
	if err != nil {
		return Result{}, wrapPrepareError(ErrInvalidExternalPath, err)
	}

	payload := req.Payload
	resolvedRevision := strings.TrimSpace(payload.Digest)
	ociTrusted := sourceType != config.SourceTypeOCI

	switch sourceType {
	case config.SourceTypeGit:
		resolvedRevision, err = p.prepareGit(req, internalRepoPath, externalRepoPath, resolvedRevision)
		if err != nil {
			p.postEarlyFailureCommitStatus(ctx, req, sourceType, resolvedRevision, payload, err)
			return Result{}, err
		}
	case config.SourceTypeOCI:
		resolvedRevision, ociTrusted, payload, err = p.prepareOCI(ctx, req, internalRepoPath, repoName)
		if err != nil {
			return Result{}, err
		}
	}

	deployConfigs, err := p.resolveDeployConfigs(req, internalRepoPath, payload.Ref)
	if err != nil {
		p.postEarlyFailureCommitStatus(ctx, req, sourceType, resolvedRevision, payload, err)
		return Result{}, err
	}

	// For OCI sources, the deploy config's reference must reflect the actual artifact tag that
	// triggered this deployment (e.g. "latest"), overriding any reference baked into the config file.
	if sourceType == config.SourceTypeOCI && req.Ref != "" {
		for _, cfg := range deployConfigs {
			cfg.Reference = req.Ref
		}
	}

	customTarget := strings.TrimSpace(req.CustomTarget)
	for _, cfg := range deployConfigs {
		cfg.Internal.ConfigTarget = customTarget
	}

	return Result{
		SourceType:    sourceType,
		RepoName:      repoName,
		PathInternal:  internalRepoPath,
		PathExternal:  externalRepoPath,
		Revision:      resolvedRevision,
		OCITrusted:    ociTrusted,
		DeployConfigs: deployConfigs,
		Payload:       payload,
	}, nil
}
