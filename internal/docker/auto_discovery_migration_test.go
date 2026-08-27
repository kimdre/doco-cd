package docker

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config/deploy"
)

func TestNormalizeAutoDiscoveryLabels(t *testing.T) {
	tests := []struct {
		name          string
		labels        map[string]string
		want          deploy.AutoDiscoveryConfig
		wantMigrated  bool
		wantCandidate bool
	}{
		{
			name: "oldest labels",
			labels: map[string]string{
				legacyAutoDiscoverLabel:       "true",
				legacyAutoDiscoverDeleteLabel: "false",
			},
			want: deploy.AutoDiscoveryConfig{
				Enabled:       true,
				Delete:        false,
				RemoveVolumes: false,
				RemoveImages:  true,
			},
			wantMigrated:  true,
			wantCandidate: true,
		},
		{
			name: "pre-consolidation delete label",
			labels: map[string]string{
				DocoCDLabels.Deployment.AutoDiscovery: "true",
				legacyAutoDiscoveryDeleteLabel:        "false",
			},
			want: deploy.AutoDiscoveryConfig{
				Enabled:       true,
				Delete:        false,
				RemoveVolumes: false,
				RemoveImages:  true,
			},
			wantMigrated:  true,
			wantCandidate: true,
		},
		{
			name: "current labels",
			labels: map[string]string{
				DocoCDLabels.Deployment.AutoDiscovery:       "true",
				DocoCDLabels.Deployment.AutoDiscoveryConfig: "{enabled: true, delete: true, remove_images: false}",
			},
			want: deploy.AutoDiscoveryConfig{
				Enabled:      true,
				Delete:       true,
				RemoveImages: false,
			},
			wantMigrated:  false,
			wantCandidate: true,
		},
		{
			name: "current disabled value takes precedence",
			labels: map[string]string{
				DocoCDLabels.Deployment.AutoDiscovery: "false",
				legacyAutoDiscoverLabel:               "true",
			},
			wantCandidate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalized, migrated := normalizeAutoDiscoveryLabels(tt.labels)
			if (normalized != nil) != tt.wantCandidate {
				t.Fatalf("candidate = %t, want %t", normalized != nil, tt.wantCandidate)
			}

			if migrated != tt.wantMigrated {
				t.Fatalf("migrated = %t, want %t", migrated, tt.wantMigrated)
			}

			if !tt.wantCandidate {
				return
			}

			for _, legacyLabel := range []string{
				legacyAutoDiscoverLabel,
				legacyAutoDiscoverDeleteLabel,
				legacyAutoDiscoveryDeleteLabel,
			} {
				if _, exists := normalized[legacyLabel]; exists {
					t.Errorf("legacy label %q was not removed", legacyLabel)
				}
			}

			got := ParseAutoDiscoveryConfig(normalized[DocoCDLabels.Deployment.AutoDiscoveryConfig])
			if got != tt.want {
				t.Errorf("config = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestNeedsSwarmAutoDiscoveryLabelMigration(t *testing.T) {
	currentLabels := map[string]string{
		DocoCDLabels.Deployment.AutoDiscovery:       "true",
		DocoCDLabels.Deployment.AutoDiscoveryConfig: "{enabled: true}",
	}

	if needsSwarmAutoDiscoveryLabelMigration(currentLabels) {
		t.Fatal("current labels should not require migration")
	}

	for _, labels := range []map[string]string{
		{legacyAutoDiscoverLabel: "true"},
		{DocoCDLabels.Deployment.AutoDiscovery: "true"},
		{
			DocoCDLabels.Deployment.AutoDiscovery:       "true",
			DocoCDLabels.Deployment.AutoDiscoveryConfig: "{enabled: true}",
			legacyAutoDiscoverLabel:                     "true",
		},
	} {
		if !needsSwarmAutoDiscoveryLabelMigration(labels) {
			t.Errorf("labels should require migration: %v", labels)
		}
	}
}
