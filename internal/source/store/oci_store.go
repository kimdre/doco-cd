// OCIStore implements Store on top of an OCI artifact reference (a fixed
// repository and tag, e.g. "ghcr.io/org/repo:latest"). See the package doc
// on git_store.go for how OCIStore's role fits into the Phase 2 plan.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/source/oci"
)

// OCIStoreOptions configures an OCIStore.
type OCIStoreOptions struct {
	// Log receives debug/info output. Defaults to slog.Default().
	Log *slog.Logger
	// ArtifactRef is the OCI reference to resolve and pull from,
	// e.g. "ghcr.io/org/repo:latest". Required.
	ArtifactRef string
	// BaseDir is the store's private directory: published artifacts live
	// under "<BaseDir>/artifacts/<digest>". Required.
	BaseDir string
	// CustomTarget selects a non-default ".doco-cd.<target>.y(a)ml" config
	// inside the artifact, matching the deployment's CustomTarget setting.
	CustomTarget string

	TrustPolicy         config.OciTrustPolicy
	TrustPolicyOverride config.OciTrustPolicyOverride
	VerifyMaxWorkers    uint
}

// OCIStore is an OCI-backed Store.
type OCIStore struct {
	opts OCIStoreOptions
}

var _ Store = (*OCIStore)(nil)

// NewOCIStore returns an OCIStore configured by opts.
func NewOCIStore(opts OCIStoreOptions) (*OCIStore, error) {
	if strings.TrimSpace(opts.ArtifactRef) == "" {
		return nil, errors.New("oci store: artifact reference is required")
	}

	if opts.BaseDir == "" {
		return nil, errors.New("oci store: base directory is required")
	}

	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	if err := sweepOrphanedTemp(opts.BaseDir); err != nil {
		opts.Log.Warn("failed to sweep orphaned artifact temp directories", slog.Any("error", err))
	}

	return &OCIStore{opts: opts}, nil
}

// Resolve resolves ref - a digest, or empty/"latest" to mean "whatever the
// store's configured tag currently points to" - against the registry, and
// verifies it against the store's cosign trust policy before returning it.
// A digest that fails verification is never returned as a Revision.
func (s *OCIStore) Resolve(ctx context.Context, ref string) (Revision, error) {
	digest, err := oci.ResolveDigest(ctx, s.opts.ArtifactRef, strings.TrimSpace(ref))
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", s.opts.ArtifactRef, err)
	}

	if err := oci.VerifyWithCosign(ctx, s.opts.ArtifactRef, digest,
		s.opts.TrustPolicy, s.opts.TrustPolicyOverride, s.opts.VerifyMaxWorkers); err != nil {
		return "", fmt.Errorf("resolve %q: %w", s.opts.ArtifactRef, err)
	}

	return Revision(digest), nil
}

// Publish pulls and extracts the artifact pinned to revision into a
// read-only artifact directory. revision is passed to the pull as the
// expected digest, so the artifact actually materialized is always the one
// Resolve verified - regardless of what the store's tag currently points to.
func (s *OCIStore) Publish(ctx context.Context, revision Revision) (Artifact, error) {
	if existing, ok, err := s.Lookup(revision); err != nil {
		return Artifact{}, err
	} else if ok {
		return existing, nil
	}

	return publishDir(s.opts.BaseDir, revision, func(dir string) error {
		pinnedRef := oci.RepositoryNameFromArtifact(s.opts.ArtifactRef) + "@" + string(revision)
		if _, err := oci.PullAndExtract(ctx,
			pinnedRef, string(revision), config.OciArtifactLayoutV1,
			dir, s.opts.CustomTarget); err != nil {
			return err
		}

		return decryptArtifact(s.opts.Log, dir)
	})
}

// Lookup returns the already-published artifact for revision, if any.
func (s *OCIStore) Lookup(revision Revision) (Artifact, bool, error) {
	return lookupArtifact(s.opts.BaseDir, revision)
}

// List returns every artifact this store has published.
func (s *OCIStore) List() ([]Artifact, error) {
	return listArtifacts(s.opts.BaseDir)
}
