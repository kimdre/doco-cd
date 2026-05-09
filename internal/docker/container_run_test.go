package docker

import (
	"testing"

	containerTypes "github.com/moby/moby/api/types/container"
)

func TestGetContainerRunAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		state  *containerTypes.State
		want   containerRunAction
		baseOK bool
	}{
		{
			name:   "missing inspect state defaults to restart",
			want:   containerRunActionRestart,
			baseOK: false,
		},
		{
			name:   "nil state defaults to restart",
			want:   containerRunActionRestart,
			baseOK: true,
		},
		{
			name:   "running container restarts",
			state:  &containerTypes.State{Running: true},
			want:   containerRunActionRestart,
			baseOK: true,
		},
		{
			name:   "created container starts",
			state:  &containerTypes.State{Running: false, Status: "created"},
			want:   containerRunActionStart,
			baseOK: true,
		},
		{
			name:   "exited container starts",
			state:  &containerTypes.State{Running: false, Status: "exited"},
			want:   containerRunActionStart,
			baseOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			inspect := containerTypes.InspectResponse{}
			if tt.baseOK {
				inspect.State = tt.state
			}

			if got := getContainerRunAction(inspect); got != tt.want {
				t.Fatalf("getContainerRunAction() = %q, want %q", got, tt.want)
			}
		})
	}
}
