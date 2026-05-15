package oci

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/sigstore/cosign/v3/pkg/cosign"
	"github.com/sigstore/cosign/v3/pkg/signature"

	"github.com/kimdre/doco-cd/internal/config"
)

var (
	ErrNoTrustRules       = errors.New("OCI trust policy has no trust rules")
	ErrVerificationFailed = errors.New("OCI artifact signature verification failed")
)

func VerifyWithCosign(ctx context.Context, artifactRef, digest string, globalPolicy config.OciTrustPolicy, override config.OciTrustPolicyOverride) error {
	effectivePolicy := config.EffectiveOciTrustPolicy(globalPolicy, override)
	if !effectivePolicy.Enabled {
		return nil
	}

	if strings.TrimSpace(digest) == "" {
		return fmt.Errorf("%w: empty digest", ErrVerificationFailed)
	}

	if len(effectivePolicy.KeylessIdentities) == 0 && len(effectivePolicy.PublicKeys) == 0 {
		return ErrNoTrustRules
	}

	ref, err := name.ParseReference(strings.TrimSpace(artifactRef), name.WeakValidation)
	if err != nil {
		return fmt.Errorf("%w: failed to parse OCI artifact reference: %v", ErrVerificationFailed, err)
	}

	verifyRef := ref.Context().Name()
	if strings.Contains(ref.Name(), "@") {
		verifyRef = strings.SplitN(ref.Name(), "@", 2)[0]
	}

	verifyRef = verifyRef + "@" + digest

	verifyRefRef, err := name.ParseReference(verifyRef, name.WeakValidation)
	if err != nil {
		return fmt.Errorf("%w: failed to parse verification reference: %v", ErrVerificationFailed, err)
	}

	trustedMaterial, err := cosign.TrustedRoot()
	if err != nil {
		return fmt.Errorf("%w: failed to load Sigstore trusted root: %v", ErrVerificationFailed, err)
	}

	var failures []string

	for _, key := range effectivePolicy.PublicKeys {
		verifier, err := signature.LoadPublicKeyRaw([]byte(strings.TrimSpace(key)), crypto.SHA256)
		if err != nil {
			failures = append(failures, "public key load failed: "+fmt.Sprintf("%v", err))
			continue
		}

		_, _, err = cosign.VerifyImageSignatures(ctx, verifyRefRef, &cosign.CheckOpts{
			SigVerifier:       verifier,
			TrustedMaterial:   trustedMaterial,
			ExperimentalOCI11: false,
		})
		if err == nil {
			return nil
		}

		failures = append(failures, "public key "+key+": "+fmt.Sprintf("%v", err))
	}

	for _, identity := range effectivePolicy.KeylessIdentities {
		_, _, err = cosign.VerifyImageSignatures(ctx, verifyRefRef, &cosign.CheckOpts{
			Identities:        []cosign.Identity{{Issuer: strings.TrimSpace(identity.Issuer), Subject: strings.TrimSpace(identity.Subject)}},
			TrustedMaterial:   trustedMaterial,
			ExperimentalOCI11: false,
		})
		if err == nil {
			return nil
		}

		failures = append(failures, "keyless "+identity.Subject+"@"+identity.Issuer+": "+fmt.Sprintf("%v", err))
	}

	return fmt.Errorf("%w: %s", ErrVerificationFailed, strings.Join(failures, "; "))
}
