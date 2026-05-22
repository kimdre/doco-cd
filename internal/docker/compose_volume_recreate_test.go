package docker

import (
	"reflect"
	"slices"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestGetMismatchVolumeNamesFromCreateError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "single mismatched volume",
			err:  stringError("Volume \"beszel-agent_socket\" exists but doesn't match configuration in compose file. Recreate (data will be lost)?"),
			want: []string{"beszel-agent_socket"},
		},
		{
			name: "multiple mismatched volumes",
			err: stringError("Volume \"vol_a\" exists but doesn't match configuration in compose file. Recreate (data will be lost)?\n" +
				"Volume \"vol_b\" exists but doesn't match configuration in compose file. Recreate (data will be lost)?"),
			want: []string{"vol_a", "vol_b"},
		},
		{
			name: "unrelated error",
			err:  stringError("failed to create service"),
			want: nil,
		},
		{
			name: "nil error",
			err:  nil,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := getMismatchVolumeNamesFromCreateError(tt.err)
			slices.Sort(got)
			slices.Sort(tt.want)

			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("getMismatchVolumeNamesFromCreateError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetRecreatableVolumeNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		project *types.Project
		want    []string
		wantErr bool
	}{
		{
			name: "collects recreatable volume names",
			project: &types.Project{
				Volumes: map[string]types.VolumeConfig{
					"socket": {
						Name: "beszel-agent_socket",
						Labels: map[string]string{
							DocoCDVolumeLabels.Recreate: "true",
						},
					},
					"cache": {
						Name: "beszel-agent_cache",
						Labels: map[string]string{
							DocoCDVolumeLabels.Recreate: "false",
						},
					},
					"state": {
						Name: "beszel-agent_state",
					},
				},
			},
			want: []string{"socket", "beszel-agent_socket"},
		},
		{
			name: "invalid label value",
			project: &types.Project{
				Volumes: map[string]types.VolumeConfig{
					"socket": {
						Labels: map[string]string{
							DocoCDVolumeLabels.Recreate: "maybe",
						},
					},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := getRecreatableVolumeNames(tt.project)
			if tt.wantErr {
				if err == nil {
					t.Fatal("getRecreatableVolumeNames() expected error, got nil")
				}

				return
			}

			if err != nil {
				t.Fatalf("getRecreatableVolumeNames() failed: %v", err)
			}

			gotSlice := got.ToSlice()
			slices.Sort(gotSlice)
			slices.Sort(tt.want)

			if !reflect.DeepEqual(gotSlice, tt.want) {
				t.Fatalf("getRecreatableVolumeNames() = %v, want %v", gotSlice, tt.want)
			}
		})
	}
}

type stringError string

func (e stringError) Error() string {
	return string(e)
}
