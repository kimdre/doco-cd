package docker

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/opencontainers/go-digest"

	"github.com/kimdre/doco-cd/internal/logger"
)

// copyProject creates a deep copy of the given project struct by marshaling it to JSON and unmarshalling it back to a new struct.
// This is necessary because some fields in the compose types are pointers, and we want to avoid modifying the original struct when adding labels.
func copyProject(orig *types.Project) (*types.Project, error) {
	b, err := json.Marshal(orig)
	if err != nil {
		return nil, err
	}

	clone := types.Project{}

	err = json.Unmarshal(b, &clone)
	if err != nil {
		return nil, err
	}

	return &clone, nil
}

// ProjectHash generates a SHA256 hash of the project configuration to be used for detecting changes in the project that may require a redeployment.
func ProjectHash(p *types.Project) string {
	pCopy, err := copyProject(p)
	if err != nil {
		slog.Error("failed to copy project for hashing", logger.ErrAttr(err))
		return ""
	}

	// Set all dynamic values to a constant value to avoid unnecessary changes in the hash when these values change but the actual configuration does not.
	for name, cfg := range pCopy.Services {
		// remove the Build config when generating the service hash
		cfg.Build = nil
		cfg.PullPolicy = ""

		cfg.Scale = nil
		if cfg.Deploy != nil {
			cfg.Deploy.Replicas = nil
		}

		cfg.DependsOn = nil
		cfg.Profiles = nil

		for l := range cfg.Labels {
			if strings.HasPrefix(l, "cd.doco.") || strings.HasPrefix(l, "com.docker.compose.") {
				cfg.Labels[l] = ""
			}
		}

		pCopy.Services[name] = cfg
	}

	for v, cfg := range pCopy.Volumes {
		for l := range cfg.Labels {
			if strings.HasPrefix(l, "cd.doco.") || strings.HasPrefix(l, "com.docker.compose.") {
				cfg.Labels[l] = ""
			}
		}

		pCopy.Volumes[v] = cfg
	}

	b, err := json.Marshal(p)
	if err != nil {
		slog.Error("failed to marshal project for hashing", logger.ErrAttr(err))
		return ""
	}

	return digest.SHA256.FromBytes(b).Encoded()
}
