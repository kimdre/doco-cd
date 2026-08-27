package docker

import (
	"context"
	"errors"
	"testing"

	swarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/deploy"
)

type autoDiscoveryMigrationTestClient struct {
	client.APIClient
	services    []swarmtypes.Service
	updateError error
	updatedIDs  []string
}

func (c *autoDiscoveryMigrationTestClient) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	return client.ServiceListResult{Items: c.services}, nil
}

func (c *autoDiscoveryMigrationTestClient) ServiceUpdate(_ context.Context, serviceID string, _ client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
	c.updatedIDs = append(c.updatedIDs, serviceID)

	return client.ServiceUpdateResult{}, c.updateError
}

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

func TestGetAndMigrateSwarmAutoDiscoveryServices_ContinuesAfterUpdateFailure(t *testing.T) {
	updateErr := errors.New("update failed")
	apiClient := &autoDiscoveryMigrationTestClient{
		updateError: updateErr,
		services: []swarmtypes.Service{
			{
				ID: "first-id",
				Spec: swarmtypes.ServiceSpec{Annotations: swarmtypes.Annotations{
					Name: "first",
					Labels: map[string]string{
						legacyAutoDiscoverLabel:      "true",
						DocoCDLabels.Deployment.Name: "first-stack",
					},
				}},
			},
			{
				ID: "second-id",
				Spec: swarmtypes.ServiceSpec{Annotations: swarmtypes.Annotations{
					Name: "second",
					Labels: map[string]string{
						legacyAutoDiscoverLabel:      "true",
						DocoCDLabels.Deployment.Name: "second-stack",
					},
				}},
			},
		},
	}

	services, err := getAndMigrateSwarmAutoDiscoveryServices(t.Context(), apiClient)
	if !errors.Is(err, updateErr) {
		t.Fatalf("error = %v, want wrapped update error", err)
	}

	if len(services) != 2 {
		t.Fatalf("services = %d, want 2", len(services))
	}

	if len(apiClient.updatedIDs) != 2 {
		t.Fatalf("updates = %v, want both services attempted", apiClient.updatedIDs)
	}
}
