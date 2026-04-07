package docker

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"gotest.tools/v3/assert"
)

func TestGetUnreferencedComposeNetworks(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Networks: map[string]types.NetworkConfig{
			"default":  {},
			"frontend": {},
			"backend":  {},
		},
		Services: types.Services{
			"web": {
				Name: "web",
				Networks: map[string]*types.ServiceNetworkConfig{
					"frontend": {},
				},
			},
			"worker": {
				Name: "worker",
			},
		},
	}

	unreferenced := getUnreferencedComposeNetworks(project)

	_, hasBackend := unreferenced["backend"]
	_, hasDefault := unreferenced["default"]
	_, hasFrontend := unreferenced["frontend"]

	assert.Assert(t, hasBackend)
	assert.Assert(t, !hasDefault)
	assert.Assert(t, !hasFrontend)
}

func TestGetUnreferencedComposeVolumes(t *testing.T) {
	t.Parallel()

	project := &types.Project{
		Volumes: map[string]types.VolumeConfig{
			"data":  {},
			"cache": {},
		},
		Services: types.Services{
			"web": {
				Name: "web",
				Volumes: []types.ServiceVolumeConfig{
					{Type: types.VolumeTypeVolume, Source: "data", Target: "/data"},
					{Type: types.VolumeTypeBind, Source: "./tmp", Target: "/tmp"},
				},
			},
		},
	}

	unreferenced := getUnreferencedComposeVolumes(project)

	_, hasCache := unreferenced["cache"]
	_, hasData := unreferenced["data"]

	assert.Assert(t, hasCache)
	assert.Assert(t, !hasData)
}
