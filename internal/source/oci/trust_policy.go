package oci

import "github.com/kimdre/doco-cd/internal/config"

// SelectTrustPolicyOverride returns deployment-level OCI trust policy overrides
// only when the deployment configuration origin is trusted.
func SelectTrustPolicyOverride(override config.OciTrustPolicyOverride, trusted bool) config.OciTrustPolicyOverride {
	if trusted {
		return override
	}

	return config.OciTrustPolicyOverride{}
}
