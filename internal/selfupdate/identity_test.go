package selfupdate

import (
	"testing"

	"github.com/docker/compose/v5/pkg/api"
)

func TestIdentityFromLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		labels     map[string]string
		wantOK     bool
		wantNumber int
	}{
		{
			name: "full compose labels",
			labels: map[string]string{
				api.ProjectLabel:         "doco-cd",
				api.ServiceLabel:         "app",
				api.ContainerNumberLabel: "2",
			},
			wantOK:     true,
			wantNumber: 2,
		},
		{
			name:   "missing service",
			labels: map[string]string{api.ProjectLabel: "doco-cd"},
		},
		{
			name:   "missing project",
			labels: map[string]string{api.ServiceLabel: "app"},
		},
		{
			name: "unparsable number still identifies the container",
			labels: map[string]string{
				api.ProjectLabel:         "doco-cd",
				api.ServiceLabel:         "app",
				api.ContainerNumberLabel: "not-a-number",
			},
			wantOK: true,
		},
		{name: "no labels at all"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := IdentityFromLabels("abc123", "doco-cd-app-1", tt.labels)

			if got.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v", got.OK, tt.wantOK)
			}

			if got.Number != tt.wantNumber {
				t.Errorf("Number = %d, want %d", got.Number, tt.wantNumber)
			}

			if got.ContainerID != "abc123" {
				t.Errorf("ContainerID = %q", got.ContainerID)
			}
		})
	}
}
