package oci

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kimdre/doco-cd/internal/config"
)

var (
	ErrCosignUnavailable  = errors.New("cosign binary is not available")
	ErrNoTrustRules       = errors.New("OCI trust policy has no trust rules")
	ErrVerificationFailed = errors.New("OCI artifact signature verification failed")
)

func VerifyWithCosign(ctx context.Context, artifactRef, digest string, appPolicy config.OciTrustPolicy, override config.OciTrustPolicyOverride) error {
	effectivePolicy := config.EffectiveOciTrustPolicy(appPolicy, override)
	if !effectivePolicy.Enabled {
		return nil
	}

	if strings.TrimSpace(digest) == "" {
		return fmt.Errorf("%w: empty digest", ErrVerificationFailed)
	}

	if len(effectivePolicy.KeylessIdentities) == 0 && len(effectivePolicy.PublicKeys) == 0 {
		return ErrNoTrustRules
	}

	if _, err := exec.LookPath("cosign"); err != nil {
		return ErrCosignUnavailable
	}

	verifyRef := artifactRef
	if strings.Contains(artifactRef, "@") {
		verifyRef = strings.SplitN(artifactRef, "@", 2)[0]
	}

	verifyRef = verifyRef + "@" + digest

	var failures []string

	for _, key := range effectivePolicy.PublicKeys {
		err := runCosignVerify(ctx, verifyRef, "--key", key)
		if err == nil {
			return nil
		}

		failures = append(failures, "public key "+key+": "+fmt.Sprintf("%v", err))
	}

	for _, identity := range effectivePolicy.KeylessIdentities {
		err := runCosignVerify(ctx, verifyRef,
			"--certificate-identity", identity.Subject,
			"--certificate-oidc-issuer", identity.Issuer,
		)
		if err == nil {
			return nil
		}

		failures = append(failures, "keyless "+identity.Subject+"@"+identity.Issuer+": "+fmt.Sprintf("%v", err))
	}

	return fmt.Errorf("%w: %s", ErrVerificationFailed, strings.Join(failures, "; "))
}

func runCosignVerify(ctx context.Context, ref string, extraArgs ...string) error {
	args := []string{"verify", "--output", "json"}
	args = append(args, extraArgs...)
	args = append(args, ref)

	cmd := exec.CommandContext(ctx, "cosign", args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}

	return nil
}
