package oci

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
)

func TestSelectTrustPolicyOverride_TrustBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		override        config.OciTrustPolicyOverride
		trusted         bool
		globalPolicy    config.OciTrustPolicy
		expectEnabled   bool
		expectVerifyNil bool
	}{
		{
			name:            "untrusted verify false is ignored and verification stays enabled",
			override:        config.OciTrustPolicyOverride{Verify: new(false)},
			trusted:         false,
			globalPolicy:    config.OciTrustPolicy{Enabled: true},
			expectEnabled:   true,
			expectVerifyNil: true,
		},
		{
			name:            "trusted verify true can enable when global verification is disabled",
			override:        config.OciTrustPolicyOverride{Verify: new(true)},
			trusted:         true,
			globalPolicy:    config.OciTrustPolicy{Enabled: false},
			expectEnabled:   true,
			expectVerifyNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			selected := SelectTrustPolicyOverride(tt.override, tt.trusted)
			if tt.expectVerifyNil != (selected.Verify == nil) {
				t.Fatalf("unexpected override verify nil state: got %v want %v", selected.Verify == nil, tt.expectVerifyNil)
			}

			effective := config.EffectiveOciTrustPolicy(tt.globalPolicy, selected)
			if effective.Enabled != tt.expectEnabled {
				t.Fatalf("unexpected effective Enabled: got %v want %v", effective.Enabled, tt.expectEnabled)
			}
		})
	}
}
