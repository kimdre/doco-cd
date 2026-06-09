package stages

import (
	"slices"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/docker"
)

func TestChangedServiceImages(t *testing.T) {
	project := &types.Project{
		Services: types.Services{
			"web":   {Name: "web", Image: "nginx:1.27"},
			"db":    {Name: "db", Image: "postgres:16"},
			"proxy": {Name: "proxy", Image: "nginx:1.27"}, // duplicate image
			"build": {Name: "build"},                      // no image (built locally)
		},
	}

	tests := []struct {
		name            string
		project         *types.Project
		changedServices []docker.Change
		want            []string
	}{
		{
			name:    "changed services map to deduped sorted images",
			project: project,
			changedServices: []docker.Change{
				{Type: "config", Services: []string{"web", "proxy"}},
				{Type: "image", Services: []string{"db"}},
			},
			want: []string{"nginx:1.27", "postgres:16"},
		},
		{
			name:    "service without image is skipped",
			project: project,
			changedServices: []docker.Change{
				{Type: "config", Services: []string{"build", "web"}},
			},
			want: []string{"nginx:1.27"},
		},
		{
			name:    "unknown service name is skipped",
			project: project,
			changedServices: []docker.Change{
				{Type: "config", Services: []string{"missing"}},
			},
			want: nil,
		},
		{
			name:            "no changed services",
			project:         project,
			changedServices: nil,
			want:            nil,
		},
		{
			name:            "nil project returns nil",
			project:         nil,
			changedServices: []docker.Change{{Type: "config", Services: []string{"web"}}},
			want:            nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &StageManager{
				Docker:      &Docker{Project: tt.project},
				DeployState: &DeploymentState{changedServices: tt.changedServices},
			}

			got := s.changedServiceImages()
			if !slices.Equal(got, tt.want) {
				t.Errorf("changedServiceImages() = %v, want %v", got, tt.want)
			}
		})
	}
}
