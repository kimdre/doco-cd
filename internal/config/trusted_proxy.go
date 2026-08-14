package config

import (
	"fmt"
	"net/netip"
	"strings"
)

// ParseTrustedProxyNetworks normalizes a comma-separated list of trusted proxy CIDR ranges.
func ParseTrustedProxyNetworks(value string) ([]netip.Prefix, error) {
	networks := make([]netip.Prefix, 0)
	seen := map[string]struct{}{}

	for entry := range strings.SplitSeq(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy network %q: expected CIDR", entry)
		}

		normalized := prefix.Masked()

		key := normalized.String()
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}

		networks = append(networks, normalized)
	}

	return networks, nil
}
