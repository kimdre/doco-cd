package docker

import (
	"encoding/json"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/opencontainers/go-digest"
)

// composeNetworkHash returns the config hash that Compose stores in the
// com.docker.compose.config-hash label of a network.
// Compose keeps this function in an internal package, so doco-cd repeats it here.
// Compose cannot change the format: it would recreate the networks of all users.
func composeNetworkHash(o *types.NetworkConfig) (string, error) {
	raw, err := json.Marshal(o)
	if err != nil {
		return "", err
	}

	return digest.SHA256.FromBytes(raw).Encoded(), nil
}

// composeVolumeHash returns the config hash that Compose stores in the
// com.docker.compose.config-hash label of a volume.
// Compose uses the "local" driver when the volume sets no driver.
func composeVolumeHash(o types.VolumeConfig) (string, error) {
	if o.Driver == "" {
		o.Driver = "local"
	}

	raw, err := json.Marshal(o)
	if err != nil {
		return "", err
	}

	return digest.SHA256.FromBytes(raw).Encoded(), nil
}
