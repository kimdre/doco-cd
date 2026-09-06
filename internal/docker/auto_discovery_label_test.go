package docker

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config/deploy"
)

// TestParseAutoDiscoveryConfigIgnoresKeyOrder prevents serialization-only drift (#1818).
func TestParseAutoDiscoveryConfigIgnoresKeyOrder(t *testing.T) {
	t.Parallel()

	want := deploy.AutoDiscoveryConfig{
		Enabled:      true,
		Delete:       true,
		RemoveImages: true,
	}

	labels := []string{
		"{enabled: true, depth: 0, delete: true, remove_volumes: false, remove_images: true}",
		"{depth: 0, enabled: true, delete: true, remove_volumes: false, remove_images: true}",
		"{remove_images: true, remove_volumes: false, delete: true, depth: 0, enabled: true}",
	}

	for _, label := range labels {
		if got := ParseAutoDiscoveryConfig(label); got != want {
			t.Errorf("ParseAutoDiscoveryConfig(%q) = %+v, want %+v", label, got, want)
		}
	}
}

func TestMarshalAutoDiscoveryConfigRoundTrip(t *testing.T) {
	t.Parallel()

	configs := []deploy.AutoDiscoveryConfig{
		{Enabled: true, Delete: true, RemoveImages: true},
		{Enabled: true, ScanDepth: 2, Delete: true, RemoveVolumes: true, RemoveImages: true},
		{Enabled: false, ScanDepth: 1},
	}

	for _, cfg := range configs {
		if got := ParseAutoDiscoveryConfig(MarshalAutoDiscoveryConfig(cfg)); got != cfg {
			t.Errorf("round trip = %+v, want %+v", got, cfg)
		}
	}
}

func TestAutoDiscoveryConfigsEqual(t *testing.T) {
	t.Parallel()

	base := deploy.AutoDiscoveryConfig{
		Enabled:      true,
		ScanDepth:    2,
		Delete:       true,
		RemoveImages: true,
	}

	tests := []struct {
		name string
		a, b deploy.AutoDiscoveryConfig
		want bool
	}{
		{name: "identical", a: base, b: base, want: true},
		{
			name: "labels written in a different key order",
			a:    base,
			b:    ParseAutoDiscoveryConfig("{depth: 2, enabled: true, delete: true, remove_volumes: false, remove_images: true}"),
			want: true,
		},
		{
			name: "differing scan depth",
			a:    base,
			b:    deploy.AutoDiscoveryConfig{Enabled: true, ScanDepth: 3, Delete: true, RemoveImages: true},
			want: false,
		},
		{
			name: "differing bool",
			a:    base,
			b:    deploy.AutoDiscoveryConfig{Enabled: true, ScanDepth: 2, Delete: false, RemoveImages: true},
			want: false,
		},
		{
			name: "zero values",
			a:    deploy.AutoDiscoveryConfig{},
			b:    deploy.AutoDiscoveryConfig{},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := AutoDiscoveryConfigsEqual(tt.a, tt.b); got != tt.want {
				t.Fatalf("AutoDiscoveryConfigsEqual(%+v, %+v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
