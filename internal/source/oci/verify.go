package oci

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/sigstore/cosign/v3/pkg/cosign"
	"github.com/sigstore/cosign/v3/pkg/signature"

	"github.com/kimdre/doco-cd/internal/config"
)

var (
	ErrNoTrustRules       = errors.New("OCI trust policy has no trust rules")
	ErrVerificationFailed = errors.New("OCI artifact signature verification failed")

	loadTrustedRoot = sync.OnceValues(cosign.TrustedRoot)
)

const (
	defaultCosignVerifyMaxWorkers = 1
	maxCosignVerifyMaxWorkers     = 10
)

func normalizeVerifyMaxWorkers(maxWorkers uint) int {
	if maxWorkers < defaultCosignVerifyMaxWorkers {
		return defaultCosignVerifyMaxWorkers
	}

	if maxWorkers > maxCosignVerifyMaxWorkers {
		return maxCosignVerifyMaxWorkers
	}

	return int(maxWorkers)
}

// toCosignIdentity converts a keyless identity from the trust policy to a Cosign identity.
func toCosignIdentity(identity config.OciKeylessIdentity) cosign.Identity {
	return cosign.Identity{
		Issuer:        strings.TrimSpace(identity.Issuer),
		Subject:       strings.TrimSpace(identity.Subject),
		SubjectRegExp: strings.TrimSpace(identity.SubjectRegexp),
	}
}

// verifyCosignEntity attempts to verify the signatures of the given reference using the provided options.
func verifyCosignEntity(ctx context.Context, ref name.Reference, opts *cosign.CheckOpts, maxWorkers uint) error {
	classicOpts := *opts
	classicOpts.MaxWorkers = normalizeVerifyMaxWorkers(maxWorkers)

	_, valid, err := cosign.VerifyImageSignatures(ctx, ref, &classicOpts)
	if err == nil && valid {
		return nil
	}

	if _, ok := errors.AsType[*cosign.ErrNoSignaturesFound](err); !ok {
		return err
	}

	bundleOpts := classicOpts
	bundleOpts.NewBundleFormat = true

	_, _, bundleErr := cosign.VerifyImageAttestations(ctx, ref, &bundleOpts)
	if bundleErr == nil {
		return nil
	}

	return fmt.Errorf("signature verify: %v; bundle verify: %v", err, bundleErr)
}

func VerifyWithCosign(ctx context.Context, artifactRef, digest string, globalPolicy config.OciTrustPolicy, override config.OciTrustPolicyOverride, maxWorkers uint) error {
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

	trustedMaterial, err := loadTrustedRoot()
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

		err = verifyCosignEntity(ctx, verifyRefRef, &cosign.CheckOpts{
			SigVerifier:       verifier,
			TrustedMaterial:   trustedMaterial,
			ExperimentalOCI11: true,
			IgnoreTlog:        effectivePolicy.IgnoreTlog,
		}, maxWorkers)
		if err == nil {
			return nil
		}

		failures = append(failures, "public key "+key+": "+fmt.Sprintf("%v", err))
	}

	for _, identity := range effectivePolicy.KeylessIdentities {
		identityMatcher := strings.TrimSpace(identity.Subject)
		if identityMatcher == "" {
			identityMatcher = "subject_regexp=" + strings.TrimSpace(identity.SubjectRegexp)
		}

		err = verifyCosignEntity(ctx, verifyRefRef, &cosign.CheckOpts{
			Identities:        []cosign.Identity{toCosignIdentity(identity)},
			TrustedMaterial:   trustedMaterial,
			ExperimentalOCI11: true,
			IgnoreTlog:        effectivePolicy.IgnoreTlog,
		}, maxWorkers)
		if err == nil {
			return nil
		}

		failures = append(failures, "keyless "+identityMatcher+"@"+identity.Issuer+": "+fmt.Sprintf("%v", err))
	}

	return fmt.Errorf("%w: %s", ErrVerificationFailed, strings.Join(failures, "; "))
}
