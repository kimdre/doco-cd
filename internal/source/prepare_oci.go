package source

import (
	"context"
	"errors"
	"path"
	"strings"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/source/oci"
	"github.com/kimdre/doco-cd/internal/source/store"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// ociPrepareResult is prepareOCI's output: the resolved immutable revision(artifact digest)
// and the read-only artifact directory materialized for it.
type ociPrepareResult struct {
	revision     string
	artifactPath string
}

// prepareOCI publishes req's verified OCI artifact and returns its digest and enriched payload.
func (p *Preparer) prepareOCI(ctx context.Context, req Request, storeBaseDir, repoName string) (ociPrepareResult, webhook.ParsedPayload, error) {
	payload := req.Payload
	result := ociPrepareResult{}

	ociStore, err := store.NewOCIStore(store.OCIStoreOptions{
		Log:                 req.Logger,
		ArtifactRef:         req.SourceRef,
		BaseDir:             storeBaseDir,
		CustomTarget:        req.CustomTarget,
		TrustPolicy:         p.appConfig.OciTrustPolicy,
		TrustPolicyOverride: config.OciTrustPolicyOverride{},
		VerifyMaxWorkers:    p.appConfig.OciVerifyMaxWorkers,
	})
	if err != nil {
		return result, payload, wrapPrepareError(ErrOCIResolveDigest, err)
	}

	revision, err := ociStore.Resolve(ctx, strings.TrimSpace(payload.Digest))
	if err != nil {
		// Resolve folds digest resolution and cosign verification into one call;
		// distinguish them here by the wrapped sentinel so callers
		// keep seeing the same error classification as before.
		if errors.Is(err, oci.ErrVerificationFailed) || errors.Is(err, oci.ErrNoTrustRules) {
			return result, payload, wrapPrepareError(ErrOCIVerify, err)
		}

		return result, payload, wrapPrepareError(ErrOCIResolveDigest, err)
	}

	artifact, err := ociStore.Publish(ctx, revision)
	if err != nil {
		return result, payload, wrapPrepareError(ErrOCIPull, err)
	}

	result.revision = string(revision)
	result.artifactPath = artifact.Path

	payload.Source = webhook.PayloadSourceOCI
	payload.Artifact = req.SourceRef
	payload.Digest = string(revision)
	payload.Trigger = string(revision)

	if payload.FullName == "" {
		payload.FullName = repoName
	}

	if payload.Name == "" {
		payload.Name = path.Base(repoName)
	}

	if payload.WebURL == "" {
		payload.WebURL = req.SourceRef
	}

	return result, payload, nil
}
