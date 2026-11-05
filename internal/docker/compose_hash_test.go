package docker

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

// TestComposeHashGoldenValues uses the expected values from the Compose v5.7.0 tests.
func TestComposeHashGoldenValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		hash func() (string, error)
		want string
	}{
		{
			name: "network",
			hash: func() (string, error) {
				return composeNetworkHash(&types.NetworkConfig{Name: "proj_default", Driver: "bridge"})
			},
			want: "eeef29d8955b1d4e9382986e213c80789065d989d7a6d035163e0180159aaac0",
		},
		{
			name: "volume",
			hash: func() (string, error) { return composeVolumeHash(types.VolumeConfig{Name: "proj_data"}) },
			want: "dd3953f0ff20e0f9044086b0483690f2cc2b4653e57aaaa5fa8ea2735da61000",
		},
		{
			name: "volume with explicit local driver",
			hash: func() (string, error) {
				return composeVolumeHash(types.VolumeConfig{Name: "proj_data", Driver: "local"})
			},
			want: "dd3953f0ff20e0f9044086b0483690f2cc2b4653e57aaaa5fa8ea2735da61000",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.hash()
			if err != nil {
				t.Fatalf("hash: %v", err)
			}

			if got != tc.want {
				t.Fatalf("hash = %s, want %s", got, tc.want)
			}
		})
	}
}
