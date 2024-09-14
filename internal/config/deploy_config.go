package config

import (
	"errors"
	"fmt"
	"os"
	"path"

	"gopkg.in/validator.v2"

	"github.com/compose-spec/compose-go/v2/cli"
)

var (
	DefaultDeploymentConfigFileNames    = []string{".doco-cd.yaml", ".doco-cd.yml"}
	DeprecatedDeploymentConfigFileNames = []string{".compose-deploy.yaml", ".compose-deploy.yml"}
	ErrConfigFileNotFound               = errors.New("configuration file not found in repository")
	ErrInvalidConfig                    = errors.New("invalid deploy configuration")
	ErrKeyNotFound                      = errors.New("key not found")
	ErrDeprecatedConfig                 = errors.New("configuration file name is deprecated, please use .doco-cd.y(a)ml instead")
)

// DeployConfig is the structure of the deployment configuration file
type DeployConfig struct {
	Name             string   `yaml:"name"`                                                                                                         // Name is the name of the docker-compose deployment / stack
	Reference        string   `yaml:"reference" default:"refs/heads/main"`                                                                          // Reference is the Git reference to the deployment, e.g. refs/heads/main or refs/tags/v1.0.0
	WorkingDirectory string   `yaml:"working_dir" default:"."`                                                                                      // WorkingDirectory is the working directory for the deployment
	ComposeFiles     []string `yaml:"compose_files" default:"[\"compose.yaml\", \"compose.yml\", \"docker-compose.yml\", \"docker-compose.yaml\"]"` // ComposeFiles is the list of docker-compose files to use
	RemoveOrphans    bool     `yaml:"remove_orphans" default:"true"`                                                                                // RemoveOrphans removes containers for services not defined in the Compose file
	ForceRecreate    bool     `yaml:"force_recreate" default:"false"`                                                                               // ForceRecreate forces the recreation/redeployment of containers even if the configuration has not changed
	ForceImagePull   bool     `yaml:"force_image_pull" default:"false"`                                                                             // ForceImagePull always pulls the latest version of the image tags you've specified if a newer version is available
	Timeout          int      `yaml:"timeout" default:"180"`                                                                                        // Timeout is the time in seconds to wait for the deployment to finish in seconds before timing out
	BuildOpts        struct {
		ForceImagePull bool              `yaml:"force_image_pull" default:"false"` // ForceImagePull always attempt to pull a newer version of the image
		Quiet          bool              `yaml:"quiet" default:"false"`            // Quiet suppresses the build output
		Args           map[string]string `yaml:"args"`                             // BuildArgs is a map of build-time arguments to pass to the build process
		NoCache        bool              `yaml:"no_cache" default:"false"`         // NoCache disables the use of the cache when building images
	} `yaml:"build_opts"` // BuildOpts is the build options for the deployment
}

// DefaultDeployConfig creates a DeployConfig with default values
func DefaultDeployConfig(name string) *DeployConfig {
	return &DeployConfig{
		Name:             name,
		Reference:        "/ref/heads/main",
		WorkingDirectory: ".",
		ComposeFiles:     cli.DefaultFileNames,
	}
}

func (c *DeployConfig) validateConfig() error {
	if c.Name == "" {
		return fmt.Errorf("%w: name", ErrKeyNotFound)
	}

	if c.Reference == "" {
		return fmt.Errorf("%w: reference", ErrKeyNotFound)
	}

	if c.WorkingDirectory == "" {
		return fmt.Errorf("%w: working_dir", ErrKeyNotFound)
	}

	if len(c.ComposeFiles) == 0 {
		return fmt.Errorf("%w: compose_files", ErrKeyNotFound)
	}

	return nil
}

// GetDeployConfig returns either the deployment configuration from the repository or the default configuration
func GetDeployConfig(repoDir, name string) (*DeployConfig, error) {
	files, err := os.ReadDir(repoDir)
	if err != nil {
		return nil, err
	}

	// Merge default and deprecated deployment config file names
	DeploymentConfigFileNames := append(DefaultDeploymentConfigFileNames, DeprecatedDeploymentConfigFileNames...)

	for _, configFile := range DeploymentConfigFileNames {
		config, err := getDeployConfigFile(repoDir, files, configFile)
		if err != nil {
			if errors.Is(err, ErrConfigFileNotFound) {
				continue
			} else {
				return nil, err
			}
		}

		if config != nil {
			if err := validator.Validate(config); err != nil {
				return nil, err
			}

			// Check if the config file name is deprecated
			for _, deprecatedConfigFile := range DeprecatedDeploymentConfigFileNames {
				if configFile == deprecatedConfigFile {
					return config, fmt.Errorf("%w: %s", ErrDeprecatedConfig, configFile)
				}
			}

			return config, nil
		}
	}

	return DefaultDeployConfig(name), nil
}

// getDeployConfigFile returns the deployment configuration from the repository or nil if not found
func getDeployConfigFile(dir string, files []os.DirEntry, configFile string) (*DeployConfig, error) {
	for _, f := range files {
		if f.IsDir() {
			continue
		}

		if f.Name() == configFile {
			// Get contents of deploy config file
			c, err := FromYAML(path.Join(dir, f.Name()))
			if err != nil {
				return nil, err
			}

			if err = c.validateConfig(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
			}

			if c != nil {
				return c, nil
			}
		}
	}

	return nil, ErrConfigFileNotFound
}
