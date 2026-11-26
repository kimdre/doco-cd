package selfupdate

import (
	"testing"

	"github.com/docker/compose/v5/pkg/api"
)

func TestIdentityFromLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		labels      map[string]string
		wantOK      bool
		wantSwarm   bool
		wantProject string
		wantService string
		wantNumber  int
	}{
		{
			name: "full compose labels",
			labels: map[string]string{
				api.ProjectLabel:         "doco-cd",
				api.ServiceLabel:         "app",
				api.ContainerNumberLabel: "2",
			},
			wantOK:      true,
			wantProject: "doco-cd",
			wantService: "app",
			wantNumber:  2,
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
			wantOK:      true,
			wantProject: "doco-cd",
			wantService: "app",
		},
		{
			name: "swarm stack task",
			labels: map[string]string{
				swarmStackNamespaceLabel: "doco-cd",
				swarmServiceNameLabel:    "doco-cd_app",
				swarmTaskNameLabel:       "doco-cd_app.3.k2j4h5g6",
			},
			wantOK:      true,
			wantSwarm:   true,
			wantProject: "doco-cd",
			wantService: "app",
			wantNumber:  3,
		},
		{
			name: "swarm global task has no numeric slot",
			labels: map[string]string{
				swarmStackNamespaceLabel: "doco-cd",
				swarmServiceNameLabel:    "doco-cd_app",
				swarmTaskNameLabel:       "doco-cd_app.nodeid123.k2j4h5g6",
			},
			wantOK:      true,
			wantSwarm:   true,
			wantProject: "doco-cd",
			wantService: "app",
		},
		{
			name: "swarm labels win over inherited compose labels",
			labels: map[string]string{
				api.ProjectLabel:         "compose-project",
				api.ServiceLabel:         "app",
				api.ContainerNumberLabel: "1",
				swarmStackNamespaceLabel: "doco-cd",
				swarmServiceNameLabel:    "doco-cd_app",
			},
			wantOK:      true,
			wantSwarm:   true,
			wantProject: "doco-cd",
			wantService: "app",
		},
		{
			name: "swarm task name belongs to another service",
			labels: map[string]string{
				swarmStackNamespaceLabel: "doco-cd",
				swarmServiceNameLabel:    "doco-cd_app",
				swarmTaskNameLabel:       "other_app.3.k2j4h5g6",
			},
			wantOK:      true,
			wantSwarm:   true,
			wantProject: "doco-cd",
			wantService: "app",
		},
		{
			name:   "swarm service outside a stack",
			labels: map[string]string{swarmServiceNameLabel: "app"},
		},
		{
			name: "swarm service name without stack prefix",
			labels: map[string]string{
				swarmStackNamespaceLabel: "doco-cd",
				swarmServiceNameLabel:    "other_app",
			},
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

			if got.Swarm != tt.wantSwarm {
				t.Errorf("Swarm = %v, want %v", got.Swarm, tt.wantSwarm)
			}

			if tt.wantOK && (got.Project != tt.wantProject || got.Service != tt.wantService) {
				t.Errorf("Project/Service = %q/%q, want %q/%q", got.Project, got.Service, tt.wantProject, tt.wantService)
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
