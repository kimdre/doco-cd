package deploy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// AutoDiscoveryConfig holds auto-discovery settings for a deployment.
type AutoDiscoveryConfig struct {
	Enable    bool `yaml:"enable" json:"enable" default:"false"` // Enable enables autodiscovery of services to deploy in the working directory
	ScanDepth int  `yaml:"depth" json:"depth" default:"0"`       // ScanDepth is the maximum depth of subdirectories to scan for docker-compose files
	Delete    bool `yaml:"delete" json:"delete" default:"true"`  // Delete removes obsolete auto-discovered deployments that are no longer present in the repository
}

func (c *AutoDiscoveryConfig) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var enabled bool
		if err := node.Decode(&enabled); err != nil {
			return errors.New("invalid auto_discovery value: expected bool or object")
		}

		c.Enable = enabled

		return nil
	case yaml.MappingNode:
		type plain AutoDiscoveryConfig

		decoded := plain(*c)
		if err := node.Decode(&decoded); err != nil {
			return err
		}

		*c = AutoDiscoveryConfig(decoded)

		return nil
	default:
		return errors.New("invalid auto_discovery value: expected bool or object")
	}
}

func (c *AutoDiscoveryConfig) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("true")) || bytes.Equal(bytes.TrimSpace(data), []byte("false")) {
		var enabled bool
		if err := json.Unmarshal(data, &enabled); err != nil {
			return errors.New("invalid auto_discovery value: expected bool or object")
		}

		c.Enable = enabled

		return nil
	}

	type plain AutoDiscoveryConfig

	decoded := plain(*c)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	*c = AutoDiscoveryConfig(decoded)

	return nil
}

// expandInlineAutoDiscoverConfigs replaces inline deployments that have auto-discovery
// enabled with the discovered deployments rooted at repoRoot.
func expandInlineAutoDiscoverConfigs(repoRoot string, deployments []*Config) ([]*Config, error) {
	expanded := make([]*Config, 0, len(deployments))

	for _, deployment := range deployments {
		if !deployment.AutoDiscovery.Enable {
			expanded = append(expanded, deployment)
			continue
		}

		discoveredConfigs, err := autoDiscoverDeployments(repoRoot, deployment)
		if err != nil {
			return nil, fmt.Errorf("failed to auto-discover deployment configurations: %w", err)
		}

		expanded = append(expanded, discoveredConfigs...)
	}

	return expanded, nil
}

// autoDiscoverDeployments scans for subdirectories containing docker-compose files
// and generates Config entries for each.
// repoRoot is the absolute path to the repository root.
// baseConfig.WorkingDirectory is treated as repo-root-relative.
func autoDiscoverDeployments(repoRoot string, baseConfig *Config) ([]*Config, error) {
	var configs []*Config

	searchPath := filepath.Join(repoRoot, baseConfig.WorkingDirectory)

	err := filepath.WalkDir(searchPath, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Calculate the depth of the current path relative to the search path
		rel, err := filepath.Rel(searchPath, p)
		if err != nil {
			return err
		}

		depth := 0
		if rel != "." {
			depth = len(strings.Split(rel, string(os.PathSeparator)))
		}

		// Skip directories that exceed the maximum depth if ScanDepth is set greater than 0
		if d.IsDir() && depth > baseConfig.AutoDiscovery.ScanDepth && baseConfig.AutoDiscovery.ScanDepth > 0 {
			return filepath.SkipDir
		}

		if !d.IsDir() {
			return nil
		}

		// Check if the directory contains any docker-compose files
		for _, composeFile := range baseConfig.ComposeFiles {
			composeFilePath := filepath.Join(p, composeFile)
			if _, err = os.Stat(composeFilePath); err == nil {
				c := &Config{}
				deepCopy(baseConfig, c)

				stackDirName := filepath.Base(p)    // Get the stack name from the directory name where the compose file is located
				repoName := filepath.Base(repoRoot) // Get the repository name from the repo root path

				if baseConfig.Name != "" && stackDirName == repoName {
					c.Name = baseConfig.Name
				} else {
					c.Name = stackDirName
				}

				c.WorkingDirectory, err = filepath.Rel(repoRoot, p)
				if err != nil {
					return err
				}

				// Check for a nested .doco-cd config file alongside the compose file and
				// merge any overridable fields from it on top of the base config copy.
				for _, cfgName := range DefaultDeploymentConfigFileNames {
					localCfgPath := filepath.Join(p, cfgName)
					if _, statErr := os.Stat(localCfgPath); statErr != nil {
						continue
					}

					localConfigs, parseErr := GetConfigFromYAML(localCfgPath, false)
					if parseErr != nil {
						return fmt.Errorf("failed to parse nested .doco-cd config at %s: %w", localCfgPath, parseErr)
					}

					if len(localConfigs) > 1 {
						return fmt.Errorf("%w: %s contains %d documents", ErrMultipleYAMLDocuments, localCfgPath, len(localConfigs))
					}

					mergeConfig(c, localConfigs[0])

					break // use first found config file name (.yaml preferred over .yml)
				}

				if err = c.Validate(); err != nil {
					return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
				}

				configs = append(configs, c)

				break
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return configs, nil
}

// mergeConfig merges Config fields from override into base, but only for fields
// tagged with `doco:"allowOverride"`. Protected fields (reference, repository_url,
// auto_discovery, git_depth) are never overridden.
// Merge semantics:
//   - Maps: merged key-by-key (override wins on key collision)
//   - Slices: replaced entirely if the override slice is non-empty
//   - Nested structs: all sub-fields are merged (parent tag opts them in)
//   - Scalars: replaced if the override holds a non-zero value.
func mergeConfig(base, override *Config) {
	mergeStructByTag(reflect.ValueOf(base).Elem(), reflect.ValueOf(override).Elem())
}

// mergeStructByTag iterates a struct's fields and merges only those tagged doco:"allowOverride".
func mergeStructByTag(base, override reflect.Value) {
	t := base.Type()
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Tag.Get("doco") != "allowOverride" {
			continue
		}

		mergeField(base.Field(i), override.Field(i))
	}
}

// mergeField applies a single field merge from override into base.
// For structs the merge recurses into all sub-fields (no tag check – parent has opted in).
func mergeField(base, override reflect.Value) {
	switch base.Kind() {
	case reflect.Map:
		if override.IsNil() || override.Len() == 0 {
			return
		}

		if base.IsNil() {
			base.Set(reflect.MakeMap(base.Type()))
		}

		for _, k := range override.MapKeys() {
			base.SetMapIndex(k, override.MapIndex(k))
		}

	case reflect.Slice:
		if override.IsNil() || override.Len() == 0 {
			return
		}

		base.Set(override)

	case reflect.Struct:
		// Recurse into all sub-fields; the parent tag already opted them in.
		mergeAllStructFields(base, override)

	default:
		// Scalar: apply only when the override holds a non-zero value.
		if !override.IsZero() {
			base.Set(override)
		}
	}
}

// mergeAllStructFields merges every field of override into base without tag checks.
func mergeAllStructFields(base, override reflect.Value) {
	for i := 0; i < base.NumField(); i++ {
		mergeField(base.Field(i), override.Field(i))
	}
}

// deepCopy creates a deep copy of a Config struct.
func deepCopy(src, dst *Config) {
	*dst = *src

	// Deep copy maps and slices
	if src.ComposeFiles != nil {
		dst.ComposeFiles = make([]string, len(src.ComposeFiles))
		copy(dst.ComposeFiles, src.ComposeFiles)
	}

	if src.EnvFiles != nil {
		dst.EnvFiles = make([]string, len(src.EnvFiles))
		copy(dst.EnvFiles, src.EnvFiles)
	}

	if src.BuildOpts.Args != nil {
		dst.BuildOpts.Args = make(map[string]string)
		maps.Copy(dst.BuildOpts.Args, src.BuildOpts.Args)
	}

	if src.Profiles != nil {
		dst.Profiles = make([]string, len(src.Profiles))
		copy(dst.Profiles, src.Profiles)
	}

	if src.ExternalSecrets != nil {
		dst.ExternalSecrets = make(map[string]secrettypes.ExternalSecretRef)
		maps.Copy(dst.ExternalSecrets, src.ExternalSecrets)
	}
}
