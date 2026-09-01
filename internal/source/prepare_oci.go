package source

import (
	"context"
	"path"
	"strings"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/source/oci"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// prepareOCI resolves req's OCI artifact digest, verifies it against the
// configured cosign trust policy, pulls and extracts it into
// internalRepoPath, and enriches the webhook payload with the resolved
// artifact metadata. It returns the resolved digest (used as the immutable
// revision), whether the artifact is trusted (always true on success - OCI
// artifacts that fail verification never reach this point), and the enriched
// payload.
func (p *Preparer) prepareOCI(ctx context.Context, req Request, internalRepoPath, repoName string) (string, bool, webhook.ParsedPayload, error) {
	payload := req.Payload

	resolvedDigest, err := oci.ResolveDigest(ctx, req.SourceRef, strings.TrimSpace(payload.Digest))
	if err != nil {
		return "", false, payload, wrapPrepareError(ErrOCIResolveDigest, err)
	}

	if err := oci.VerifyWithCosign(ctx, req.SourceRef, resolvedDigest, p.appConfig.OciTrustPolicy, config.OciTrustPolicyOverride{}, p.appConfig.OciVerifyMaxWorkers); err != nil {
		return "", false, payload, wrapPrepareError(ErrOCIVerify, err)
	}

	pullResult, err := oci.PullAndExtract(ctx,
		req.SourceRef, resolvedDigest, config.OciArtifactLayoutV1,
		internalRepoPath, req.CustomTarget)
	if err != nil {
		return "", false, payload, wrapPrepareError(ErrOCIPull, err)
	}

	payload.Source = webhook.PayloadSourceOCI
	payload.Artifact = req.SourceRef
	payload.Digest = pullResult.Digest
	payload.Trigger = pullResult.Digest

	if payload.FullName == "" {
		payload.FullName = repoName
	}

	if payload.Name == "" {
		payload.Name = path.Base(repoName)
	}

	if payload.WebURL == "" {
		payload.WebURL = req.SourceRef
	}

	return pullResult.Digest, true, payload, nil
}
