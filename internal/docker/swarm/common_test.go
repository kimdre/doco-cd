package swarm

import (
	"testing"
)

func TestSwarmModeEnabled(t *testing.T) {
	t.Parallel()

	prevDisable := disableSwarmFeature.Load()
	prevMode := modeEnabled.Load()

	t.Cleanup(func() {
		disableSwarmFeature.Store(prevDisable)
		modeEnabled.Store(prevMode)
	})

	SetDisableSwarmFeature(false)

	dockerCli := getDockerClient(t)

	if err := RefreshModeEnabled(t.Context(), dockerCli); err != nil {
		t.Fatal(err)
	}

	want, err := ResolveModeEnabled(t.Context(), dockerCli)
	if err != nil {
		t.Fatal(err)
	}

	if got := GetModeEnabled(); got != want {
		t.Fatalf("GetModeEnabled() = %v, want %v", got, want)
	}
}

func Test_getModeEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		disableSwarmFeature bool
		modeEnabled         bool
		want                bool
	}{
		{
			name:                "disable swarm feature=true, modeEnabled=true",
			disableSwarmFeature: true,
			modeEnabled:         true,
			want:                false,
		},
		{
			name:                "disable swarm feature=true, modeEnabled=false",
			disableSwarmFeature: true,
			modeEnabled:         false,
			want:                false,
		},
		{
			name:                "disable swarm feature=false, modeEnabled=true",
			disableSwarmFeature: false,
			modeEnabled:         true,
			want:                true,
		},
		{
			name:                "disable swarm feature=false, modeEnabled=false",
			disableSwarmFeature: false,
			modeEnabled:         false,
			want:                false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getModeEnabled(tt.disableSwarmFeature, tt.modeEnabled)
			if got != tt.want {
				t.Errorf("getModeEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}
