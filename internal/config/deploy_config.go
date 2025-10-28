package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/creasty/defaults"
	"go.yaml.in/yaml/v3"
	"gopkg.in/validator.v2"
)

var (
	DefaultDeploymentConfigFileNames = []string{".doco-cd.yaml", ".doco-cd.yml"}
	CustomDeploymentConfigFileNames  = []string{".doco-cd.%s.yaml", ".doco-cd.%s.yml"}
	ErrConfigFileNotFound            = errors.New("configuration file not found in repository")
	ErrDuplicateProjectName          = errors.New("duplicate project/stack name found in configuration file")
	ErrInvalidConfig                 = errors.New("invalid deploy configuration")
	ErrKeyNotFound                   = errors.New("key not found")
	ErrInvalidFilePath               = errors.New("invalid file path")
)

const DefaultReference = "refs/heads/main"

// DeployConfig is the structure of the deployment configuration file.
type DeployConfig struct {
	Name               string   `yaml:"name"`                                                                                                         // Name of the docker-compose deployment / stack
	RepositoryUrl      HttpUrl  `yaml:"repository_url" default:"" validate:"httpUrl"`                                                                 // RepositoryUrl is the http URL of the Git repository to deploy
	WebhookEventFilter string   `yaml:"webhook_filter" default:""`                                                                                    // WebhookEventFilter is a regular expression to whitelist deployment triggers based on the webhook event payload (e.g., branch like "^refs/heads/main$" or "main", tag like "^refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$" or "v[0-9]+\.[0-9]+\.[0-9]+")
	Reference          string   `yaml:"reference" default:""`                                                                                         // Reference is the Git reference to the deployment, e.g., refs/heads/main, main, refs/tags/v1.0.0 or v1.0.0
	WorkingDirectory   string   `yaml:"working_dir" default:"."`                                                                                      // WorkingDirectory is the working directory for the deployment
	ComposeFiles       []string `yaml:"compose_files" default:"[\"compose.yaml\", \"compose.yml\", \"docker-compose.yml\", \"docker-compose.yaml\"]"` // ComposeFiles is the list of docker-compose files to use
	RemoveOrphans      bool     `yaml:"remove_orphans" default:"true"`                                                                                // RemoveOrphans removes containers for services not defined in the Compose file
	ForceRecreate      bool     `yaml:"force_recreate" default:"false"`                                                                               // ForceRecreate forces the recreation/redeployment of containers even if the configuration has not changed
	ForceImagePull     bool     `yaml:"force_image_pull" default:"false"`                                                                             // ForceImagePull always pulls the latest version of the image tags you've specified if a newer version is available
	Timeout            int      `yaml:"timeout" default:"180"`                                                                                        // Timeout is the time in seconds to wait for the deployment to finish in seconds before timing out
	BuildOpts          struct {
		ForceImagePull bool              `yaml:"force_image_pull" default:"false"` // ForceImagePull always attempt to pull a newer version of the image
		Quiet          bool              `yaml:"quiet" default:"false"`            // Quiet suppresses the build output
		Args           map[string]string `yaml:"args"`                             // BuildArgs is a map of build-time arguments to pass to the build process
		NoCache        bool              `yaml:"no_cache" default:"false"`         // NoCache disables the use of the cache when building images
	} `yaml:"build_opts"` // BuildOpts is the build options for the deployment
	Destroy     bool `yaml:"destroy" default:"false"` // Destroy removes the deployment and all its resources from the Docker host
	DestroyOpts struct {
		RemoveVolumes bool `yaml:"remove_volumes" default:"true"` // RemoveVolumes removes the volumes used by the deployment (always enabled in docker swarm mode)
		RemoveImages  bool `yaml:"remove_images" default:"true"`  // RemoveImages removes the images used by the deployment (currently not supported in docker swarm mode)
		RemoveRepoDir bool `yaml:"remove_dir" default:"true"`     // RemoveRepoDir removes the repository directory after the deployment is destroyed
	} `yaml:"destroy_opts"` // DestroyOpts is the destroy options for the deployment
	Profiles        []string          `yaml:"profiles" default:"[]"` // Profiles is a list of profiles to use for the deployment, e.g., ["dev", "prod"]. See https://docs.docker.com/compose/how-tos/profiles/
	ExternalSecrets map[string]string `yaml:"external_secrets"`      // ExternalSecrets maps env vars to secret IDs/keys for injecting secrets from external providers like Bitwarden SM at deployment, e.g. {"DB_PASSWORD": "138e3697-ed58-431c-b866-b3550066343a"}
}

// DefaultDeployConfig creates a DeployConfig with default values.
func DefaultDeployConfig(name, reference string) *DeployConfig {
	return &DeployConfig{
		Name:             name,
		Reference:        reference,
		WorkingDirectory: ".",
		ComposeFiles:     cli.DefaultFileNames,
	}
}

func (c *DeployConfig) validateConfig() error {
	if c.Name == "" {
		return fmt.Errorf("%w: name", ErrKeyNotFound)
	}

	c.WorkingDirectory = filepath.Clean(c.WorkingDirectory)
	if !filepath.IsLocal(c.WorkingDirectory) {
		c.WorkingDirectory = filepath.Join(".", c.WorkingDirectory)
	}

	if len(c.ComposeFiles) == 0 {
		return fmt.Errorf("%w: compose_files", ErrKeyNotFound)
	}

	cleanComposeFiles := make([]string, 0, len(c.ComposeFiles))
	// Sanitize the compose file path
	for _, file := range c.ComposeFiles {
		cleaned := filepath.Clean(file)
		if !filepath.IsLocal(cleaned) {
			return fmt.Errorf("%w: %s", ErrInvalidFilePath, file)
		}

		// Check if the filename contains any path
		if filepath.Base(cleaned) != cleaned {
			return fmt.Errorf("%w: %s", ErrInvalidFilePath, file)
		}

		cleanComposeFiles = append(cleanComposeFiles, cleaned)
	}

	c.ComposeFiles = cleanComposeFiles

	return nil
}

func (c *DeployConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	err := defaults.Set(c)
	if err != nil {
		return err
	}

	type Plain DeployConfig

	if err := unmarshal((*Plain)(c)); err != nil {
		return err
	}

	return nil
}

// GetDeployConfigFromYAML reads a YAML file and unmarshals it into a slice of DeployConfig structs.
func GetDeployConfigFromYAML(f string) ([]*DeployConfig, error) {
	b, err := os.ReadFile(f) // #nosec G304
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %v", err)
	}

	// Read all yaml documents in the file and unmarshal them into a slice of DeployConfig structs
	dec := yaml.NewDecoder(bytes.NewReader(b))

	var configs []*DeployConfig

	for {
		var c DeployConfig

		err = dec.Decode(&c)
		if err != nil {
			if err == io.EOF {
				break
			}

			return nil, fmt.Errorf("failed to decode yaml: %v", err)
		}

		configs = append(configs, &c)
	}

	if len(configs) == 0 {
		return nil, errors.New("no yaml documents found in file")
	}

	return configs, nil
}

// GetDeployConfigs returns either the deployment configuration from the repository or the default configuration.
func GetDeployConfigs(repoDir, name, customTarget, reference string) ([]*DeployConfig, error) {
	files, err := os.ReadDir(repoDir)
	if err != nil {
		return nil, err
	}

	var DeploymentConfigFileNames []string

	if reference == "" {
		reference = DefaultReference
	}

	if customTarget != "" {
		for _, configFile := range CustomDeploymentConfigFileNames {
			DeploymentConfigFileNames = append(DeploymentConfigFileNames, fmt.Sprintf(configFile, customTarget))
		}
	} else {
		DeploymentConfigFileNames = DefaultDeploymentConfigFileNames
	}

	var configs []*DeployConfig
	for _, configFile := range DeploymentConfigFileNames {
		configs, err = getDeployConfigsFromFile(repoDir, files, configFile)
		if err != nil {
			if errors.Is(err, ErrConfigFileNotFound) {
				continue
			}

			return nil, err
		}

		if configs != nil {
			if err = validator.Validate(configs); err != nil {
				return nil, err
			}

			// Check if the stack/project names are not unique
			err = validateUniqueProjectNames(configs)
			if err != nil {
				return nil, err
			}

			for _, c := range configs {
				// If the reference is not already set in the deployment config file, set it to the current reference
				if c.Reference == "" {
					c.Reference = reference
				}
			}

			return configs, nil
		}
	}

	if customTarget != "" {
		return nil, ErrConfigFileNotFound
	}

	return []*DeployConfig{DefaultDeployConfig(name, reference)}, nil
}

// getDeployConfigsFromFile returns the deployment configurations from the repository or nil if not found.
func getDeployConfigsFromFile(dir string, files []os.DirEntry, configFile string) ([]*DeployConfig, error) {
	for _, f := range files {
		if f.IsDir() {
			continue
		}

		if f.Name() == configFile {
			// Get contents of deploy config file
			configs, err := GetDeployConfigFromYAML(path.Join(dir, f.Name()))
			if err != nil {
				return nil, err
			}

			// Validate all deploy configs
			for _, c := range configs {
				if err = c.validateConfig(); err != nil {
					return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
				}
			}

			if configs != nil {
				return configs, nil
			}
		}
	}

	return nil, ErrConfigFileNotFound
}

// validateUniqueProjectNames checks if the project names in the configs are unique.
func validateUniqueProjectNames(configs []*DeployConfig) error {
	names := make(map[string]bool)
	for _, config := range configs {
		if names[config.Name] {
			return fmt.Errorf("%w: %s", ErrDuplicateProjectName, config.Name)
		}

		names[config.Name] = true
	}

	return nil
}
