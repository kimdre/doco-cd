package config

import (
	"net/netip"
	"testing"
)

func TestParseTrustedProxyNetworks(t *testing.T) {
	networks, err := ParseTrustedProxyNetworks(" 127.0.0.1/8, 10.0.0.0/8,10.0.0.0/8, ::1/128 ")
	if err != nil {
		t.Fatalf("expected trusted proxy networks to parse, got %v", err)
	}

	got := make([]string, 0, len(networks))
	for _, network := range networks {
		got = append(got, network.String())
	}

	want := []string{"127.0.0.0/8", "10.0.0.0/8", "::1/128"}
	if len(got) != len(want) {
		t.Fatalf("expected %d networks, got %d (%v)", len(want), len(got), got)
	}

	for i, expected := range want {
		if got[i] != expected {
			t.Fatalf("expected network %d to be %q, got %q", i, expected, got[i])
		}
	}
}

func TestParseTrustedProxyNetworksEmpty(t *testing.T) {
	networks, err := ParseTrustedProxyNetworks(" , ")
	if err != nil {
		t.Fatalf("expected empty list to parse, got %v", err)
	}

	if len(networks) != 0 {
		t.Fatalf("expected no networks, got %v", networks)
	}
}

func TestParseTrustedProxyNetworksRejectsInvalidCIDR(t *testing.T) {
	if _, err := ParseTrustedProxyNetworks("localhost"); err == nil {
		t.Fatal("expected invalid CIDR to be rejected")
	}
}

func TestParseTrustedProxyNetworksReturnsMaskedPrefixes(t *testing.T) {
	networks, err := ParseTrustedProxyNetworks("192.168.1.10/16")
	if err != nil {
		t.Fatalf("expected trusted proxy network to parse, got %v", err)
	}

	if len(networks) != 1 {
		t.Fatalf("expected 1 network, got %d", len(networks))
	}

	expected := netip.MustParsePrefix("192.168.0.0/16")
	if networks[0] != expected {
		t.Fatalf("expected masked network %s, got %s", expected, networks[0])
	}
}
