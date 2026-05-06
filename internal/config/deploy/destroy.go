package deploy

import (
	"bytes"
	"encoding/json"
	"errors"

	"go.yaml.in/yaml/v3"
)

// DestroyConfig holds options for destroying a deployment.
type DestroyConfig struct {
	Enabled       bool `yaml:"enabled" json:"enabled" default:"false"`              // Enabled removes the deployment and all its resources from the Docker host
	RemoveVolumes bool `yaml:"remove_volumes" json:"remove_volumes" default:"true"` // RemoveVolumes removes the volumes used by the deployment (always enabled in docker swarm mode)
	RemoveImages  bool `yaml:"remove_images" json:"remove_images" default:"true"`   // RemoveImages removes the images used by the deployment (currently not supported in docker swarm mode)
	RemoveRepoDir bool `yaml:"remove_dir" json:"remove_dir" default:"true"`         // RemoveRepoDir removes the repository directory after the deployment is destroyed
}

func (c *DestroyConfig) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var enabled bool
		if err := node.Decode(&enabled); err != nil {
			return errors.New("invalid destroy value: expected bool or object")
		}

		c.Enabled = enabled

		return nil
	case yaml.MappingNode:
		type plain DestroyConfig

		decoded := plain(*c)
		if err := node.Decode(&decoded); err != nil {
			return err
		}

		*c = DestroyConfig(decoded)

		return nil
	default:
		return errors.New("invalid destroy value: expected bool or object")
	}
}

func (c *DestroyConfig) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("true")) || bytes.Equal(bytes.TrimSpace(data), []byte("false")) {
		var enabled bool
		if err := json.Unmarshal(data, &enabled); err != nil {
			return errors.New("invalid destroy value: expected bool or object")
		}

		c.Enabled = enabled

		return nil
	}

	type plain DestroyConfig

	decoded := plain(*c)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	*c = DestroyConfig(decoded)

	return nil
}
