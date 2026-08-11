package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type OciKeylessIdentity struct {
	Issuer        string `yaml:"issuer" json:"issuer"`
	Subject       string `yaml:"subject" json:"subject"`
	SubjectRegexp string `yaml:"subject_regexp" json:"subject_regexp"`
}

type OciTrustPolicy struct {
	Enabled           bool                 `yaml:"enabled" json:"enabled"`
	KeylessIdentities []OciKeylessIdentity `yaml:"keyless_identities" json:"keyless_identities"`
	PublicKeys        []string             `yaml:"public_keys" json:"public_keys"`
	IgnoreTlog        bool                 `yaml:"ignore_tlog" json:"ignore_tlog"`
}

type OciTrustPolicyOverride struct {
	Verify            *bool                `yaml:"verify" json:"verify"`
	KeylessIdentities []OciKeylessIdentity `yaml:"keyless_identities" json:"keyless_identities"`
	PublicKeys        []string             `yaml:"public_keys" json:"public_keys"`
	IgnoreTlog        *bool                `yaml:"ignore_tlog" json:"ignore_tlog"`
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

	if override.IgnoreTlog != nil {
		p.IgnoreTlog = *override.IgnoreTlog
	}

	return NormalizeOciTrustPolicy(p)
}

// ParseOciInsecureRegistries normalizes a comma-separated list of registry host[:port] entries.
func ParseOciInsecureRegistries(value string) ([]string, error) {
	registries := make([]string, 0)
	seen := map[string]struct{}{}

	for entry := range strings.SplitSeq(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		parsed, err := url.Parse("//" + entry)
		if err != nil || parsed.Host != entry || parsed.Hostname() == "" ||
			strings.ContainsAny(entry, "/?#@") {
			return nil, fmt.Errorf("invalid OCI insecure registry %q: expected host[:port]", entry)
		}

		if strings.Contains(entry, ":") {
			port := parsed.Port()
			if port == "" {
				return nil, fmt.Errorf("invalid OCI insecure registry %q: expected host[:port]", entry)
			}

			portNumber, err := strconv.ParseUint(port, 10, 16)
			if err != nil || portNumber == 0 {
				return nil, fmt.Errorf("invalid OCI insecure registry %q: invalid port", entry)
			}
		}

		normalized := strings.ToLower(entry)
		if _, ok := seen[normalized]; ok {
			continue
		}

		seen[normalized] = struct{}{}
		registries = append(registries, normalized)
	}

	return registries, nil
}
