package config

import "strings"

type OciKeylessIdentity struct {
	Issuer        string `yaml:"issuer" json:"issuer"`
	Subject       string `yaml:"subject" json:"subject"`
	SubjectRegexp string `yaml:"subject_regexp" json:"subject_regexp"`
}

type OciTrustPolicy struct {
	Enabled           bool                 `yaml:"enabled" json:"enabled"`
	KeylessIdentities []OciKeylessIdentity `yaml:"keyless_identities" json:"keyless_identities"`
	PublicKeys        []string             `yaml:"public_keys" json:"public_keys"`
}

type OciTrustPolicyOverride struct {
	Verify            *bool                `yaml:"verify" json:"verify"`
	KeylessIdentities []OciKeylessIdentity `yaml:"keyless_identities" json:"keyless_identities"`
	PublicKeys        []string             `yaml:"public_keys" json:"public_keys"`
}

func NormalizeOciTrustPolicy(p OciTrustPolicy) OciTrustPolicy {
	if !p.Enabled {
		return p
	}

	out := p
	for i, id := range out.KeylessIdentities {
		out.KeylessIdentities[i].Issuer = strings.TrimSpace(id.Issuer)
		out.KeylessIdentities[i].Subject = strings.TrimSpace(id.Subject)
		out.KeylessIdentities[i].SubjectRegexp = strings.TrimSpace(id.SubjectRegexp)
	}

	keys := make([]string, 0, len(out.PublicKeys))
	for _, k := range out.PublicKeys {
		k = strings.TrimSpace(k)
		if k != "" {
			keys = append(keys, k)
		}
	}

	out.PublicKeys = keys

	return out
}

func EffectiveOciTrustPolicy(global OciTrustPolicy, override OciTrustPolicyOverride) OciTrustPolicy {
	p := NormalizeOciTrustPolicy(global)

	if override.Verify != nil {
		// A global enabled trust policy cannot be downgraded by per-deployment config.
		p.Enabled = p.Enabled || *override.Verify
	}

	if len(override.KeylessIdentities) > 0 {
		p.KeylessIdentities = override.KeylessIdentities
	}

	if len(override.PublicKeys) > 0 {
		p.PublicKeys = override.PublicKeys
	}

	return NormalizeOciTrustPolicy(p)
}
