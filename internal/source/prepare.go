package source

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/prometheus"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/source/oci"
)

// Prepare resolves a Git repository or OCI artifact into a deployable source. It validates the source, materializes its
// immutable revision, resolves deployment configuration, and reports early Git/configuration failures.
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

	// Hold the GC gate through deployment so another process cannot scan this
	// store between publication and the deployment labels becoming visible.
	unlockGC, err := sourcecache.AcquireSharedGCPathLock(internalRepoPath)
	if err != nil {
		return Result{}, wrapPrepareError(ErrPrepare, fmt.Errorf("acquire artifact GC lock: %w", err))
	}

	transferGCLock := false
	defer func() {
		if !transferGCLock {
			unlockGC()
		}
	}()

	// Shared, not exclusive: GitStore/OCIStore each lock their own mutation
	// (mirror fetch, artifact publish) internally, so any number of Prepare
	// calls for this repository - at the same or different revisions - may run concurrently here.
	// This only needs to exclude a concurrent destroy of the repository directory itself,
	// via AcquireExclusivePathLock (see stage_3_destroy.go).
	unlockSource := sourcecache.AcquireSharedPathLock(internalRepoPath)
	defer unlockSource()

	payload := req.Payload
	resolvedRevision := strings.TrimSpace(payload.Digest)
	ociTrusted := sourceType != config.SourceTypeOCI

	// Result paths point to the browsable published artifact, including configs that override RepositoryUrl.
	//
	// gitMirrorDir lets GetConfigs resolve other references without requiring the artifact directory to be a Git repository.
	resultPathInternal := internalRepoPath
	resultPathExternal := externalRepoPath

	var gitMirrorDir string

	sourceStartedAt := time.Now()

	switch sourceType {
	case config.SourceTypeGit:
		gitResult, gitErr := p.prepareGit(ctx, req, internalRepoPath, resolvedRevision)
		if gitErr != nil {
			p.postEarlyFailureCommitStatus(ctx, req, sourceType, gitResult.revision, payload, gitErr)
			return Result{}, gitErr
		}

		req.Logger.Info("resolved and published repository content",
			slog.String("revision", gitResult.revision),
			slog.String("elapsed_time", time.Since(sourceStartedAt).Truncate(time.Millisecond).String()))

		resolvedRevision = gitResult.revision
		gitMirrorDir = gitResult.mirrorDir
		resultPathInternal = gitResult.artifactPath

		rel, relErr := filepath.Rel(internalRepoPath, gitResult.artifactPath)
		if relErr != nil {
			return Result{}, wrapPrepareError(ErrInvalidExternalPath, relErr)
		}

		resultPathExternal = filepath.Join(externalRepoPath, rel)
	case config.SourceTypeOCI:
		ociResult, ociPayload, ociErr := p.prepareOCI(ctx, req, internalRepoPath, repoName)
		if ociErr != nil {
			return Result{}, ociErr
		}

		payload = ociPayload
		resolvedRevision = ociResult.revision
		ociTrusted = true
		resultPathInternal = ociResult.artifactPath

		req.Logger.Info("resolved and published artifact content",
			slog.String("revision", resolvedRevision),
			slog.String("elapsed_time", time.Since(sourceStartedAt).Truncate(time.Millisecond).String()))

		rel, relErr := filepath.Rel(internalRepoPath, ociResult.artifactPath)
		if relErr != nil {
			return Result{}, wrapPrepareError(ErrInvalidExternalPath, relErr)
		}

		resultPathExternal = filepath.Join(externalRepoPath, rel)
	}

	// Mark the resolved revision in use before the source path lock is
	// released, so there is no window in which the store has handed back an
	// artifact directory that artifact garbage collection (internal/gc)
	// could still consider unreferenced - Sweep takes the matching exclusive
	// lock on this directory, so it cannot run between the two. The caller
	// takes ownership of the marker via Result.Release; every error path
	// below releases it here instead.
	releaseInFlight := MarkInFlight(repoName, resolvedRevision)

	defer func() {
		if retErr != nil {
			releaseInFlight()
		}
	}()

	deployConfigsStartedAt := time.Now()

	deployConfigs, err := p.resolveDeployConfigs(ctx, req, resultPathInternal, gitMirrorDir, resolvedRevision, payload.Ref)
	if err != nil {
		p.postEarlyFailureCommitStatus(ctx, req, sourceType, resolvedRevision, payload, err)
		return Result{}, err
	}

	req.Logger.Info("resolved deploy configs",
		slog.Int("count", len(deployConfigs)),
		slog.String("elapsed_time", time.Since(deployConfigsStartedAt).Truncate(time.Millisecond).String()))

	// For OCI sources, the deploy config's reference must reflect the actual artifact tag that
	// triggered this deployment (e.g. "latest"). A deployment-level Git repository keeps its configured Git reference.
	if sourceType == config.SourceTypeOCI {
		ociRef := req.Ref
		if ociRef == "" {
			ociRef = oci.TagFromArtifact(req.SourceRef)
		}

		applyOCIReference(deployConfigs, ociRef)
	}

	customTarget := strings.TrimSpace(req.CustomTarget)
	for _, cfg := range deployConfigs {
		cfg.Internal.ConfigTarget = customTarget
	}

	releaseArtifact := func() {
		releaseInFlight()
		unlockGC()
	}
	transferGCLock = true

	req.Logger.Info("source prepared",
		slog.String("source_type", sourceLabel),
		slog.String("revision", resolvedRevision),
		slog.String("elapsed_time", time.Since(startedAt).Truncate(time.Millisecond).String()))

	return Result{
		SourceType:    sourceType,
		RepoName:      repoName,
		PathInternal:  resultPathInternal,
		PathExternal:  resultPathExternal,
		Revision:      resolvedRevision,
		MirrorDir:     gitMirrorDir,
		OCITrusted:    ociTrusted,
		DeployConfigs: deployConfigs,
		Payload:       payload,
		release:       releaseArtifact,
	}, nil
}

func applyOCIReference(configs []*deploy.Config, ref string) {
	if ref == "" {
		return
	}

	for _, cfg := range configs {
		if cfg.RepositoryUrl == "" {
			cfg.Reference = ref
		}
	}
}
