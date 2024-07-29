package config

import (
	"errors"
	"fmt"
	"os"
	"path"

	"github.com/compose-spec/compose-go/v2/cli"

	"github.com/go-playground/webhooks/v6/github"
)

var (
	DefaultDeploymentConfigFileNames = []string{".compose-deploy.yaml", ".compose-deploy.yml"}
	ErrConfigFileNotFound            = errors.New("configuration file not found in repository")
	ErrInvalidConfig                 = errors.New("invalid deploy configuration")
	ErrKeyNotFound                   = errors.New("key not found")
)

// DeployConfig is the structure of the deployment configuration file
type DeployConfig struct {
	Name                string   `yaml:"name"`                                                                                                         // Name is the name of the docker-compose deployment / stack
	Reference           string   `yaml:"reference" default:"refs/heads/main"`                                                                          // Reference is the Git reference to the deployment, e.g. refs/heads/main or refs/tags/v1.0.0
	WorkingDirectory    string   `yaml:"working_dir" default:"."`                                                                                      // WorkingDirectory is the working directory for the deployment
	ComposeFiles        []string `yaml:"compose_files" default:"[\"compose.yaml\", \"compose.yml\", \"docker-compose.yml\", \"docker-compose.yaml\"]"` // ComposeFiles is the list of docker-compose files to use
	SkipTLSVerification bool     `yaml:"skip_tls_verify" default:"false"`                                                                              // SkipTLSVerification skips the TLS verification
	RemoveOrphans       bool     `yaml:"remove_orphans" default:"true"`                                                                                // RemoveOrphans removes containers for services not defined in the Compose file
	Timeout             int      `yaml:"timeout" default:"300"`                                                                                        // Timeout is the time to wait for the deployment to finish in seconds before timing out
}

// DefaultDeployConfig creates a DeployConfig with default values
func DefaultDeployConfig(name string) *DeployConfig {
	return &DeployConfig{
		Name:                name,
		Reference:           "/ref/heads/main",
		WorkingDirectory:    ".",
		ComposeFiles:        cli.DefaultFileNames,
		SkipTLSVerification: false,
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
func GetDeployConfig(repoDir string, event github.PushPayload) (*DeployConfig, error) {
	files, err := os.ReadDir(repoDir)
	if err != nil {
		return nil, err
	}

	for _, configFile := range DefaultDeploymentConfigFileNames {
		config, err := getDeployConfigFile(repoDir, files, configFile)
		if err != nil {
			continue
			// return nil, err
		}

		if config != nil {
			return config, nil
		}
	}

	return DefaultDeployConfig(event.Repository.Name), nil
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
