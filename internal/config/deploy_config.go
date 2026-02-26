package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/creasty/defaults"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"go.yaml.in/yaml/v3"
	"gopkg.in/validator.v2"

	gitInternal "github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/logger"
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
	EnvFiles           []string `yaml:"env_files" default:"[\".env\"]"`                                                                               // EnvFiles is the list of dotenv files to use for variable interpolation
	RemoveOrphans      bool     `yaml:"remove_orphans" default:"true"`                                                                                // RemoveOrphans removes containers for services not defined in the Compose file
	PruneImages        bool     `yaml:"prune_images" default:"true"`                                                                                  // PruneImages removes images that are no longer used by any service in the deployment or any other running container
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
	Profiles         []string          `yaml:"profiles" default:"[]"`         // Profiles is a list of profiles to use for the deployment, e.g., ["dev", "prod"]. See https://docs.docker.com/compose/how-tos/profiles/
	ExternalSecrets  map[string]string `yaml:"external_secrets"`              // ExternalSecrets maps env vars to secret IDs/keys for injecting secrets from external providers like Bitwarden SM at deployment, e.g. {"DB_PASSWORD": "138e3697-ed58-431c-b866-b3550066343a"}
	AutoDiscover     bool              `yaml:"auto_discover" default:"false"` // AutoDiscover enables autodiscovery of services to deploy in the working directory by checking for subdirectories with docker-compose files
	AutoDiscoverOpts struct {
		ScanDepth int  `yaml:"depth" default:"0"`     // ScanDepth is the maximum depth of subdirectories to scan for docker-compose files
		Delete    bool `yaml:"delete" default:"true"` // Delete removes obsolete auto-discovered deployments that are no longer present in the repository
	} `yaml:"auto_discover_opts"` // AutoDiscoverOpts are options for the autodiscovery feature
	Internal struct {
		Environment map[string]string // Environment stores environment variables from local env_files entries (if RepositoryUrl to set) for the deployment for interpolating variables in the compose files
		Hash        string            `yaml:"-"` // Hash is a hash of the DeployConfig struct (without changing the order of its elements)
	} // Internal holds internal configuration values that are not set by the user
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

// LogValue implements the slog.LogValuer interface for DeployConfig.
func (c DeployConfig) LogValue() slog.Value {
	return logger.BuildLogValue(c, "Internal")
}

func (c *DeployConfig) validateConfig() error {
	if c.Name == "" && !c.AutoDiscover {
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

		if filepath.IsAbs(cleaned) {
			return fmt.Errorf("%w: %s", ErrInvalidFilePath, file)
		}

		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("%w: %s", ErrInvalidFilePath, file)
		}

		full := filepath.Join(c.WorkingDirectory, cleaned)

		rel, err := filepath.Rel(c.WorkingDirectory, full)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidFilePath, file)
		}

		if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
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

// Hash returns a hash of the DeployConfig struct (without changing the order of its elements).
func (c *DeployConfig) Hash() (string, error) {
	data, err := yaml.Marshal(c)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// GetDeployConfigFromYAML reads a YAML file and unmarshals it into a slice of DeployConfig structs.
func GetDeployConfigFromYAML(f string) ([]*DeployConfig, error) {
	b, err := os.ReadFile(f) // #nosec G304
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %v", err)
	}

	// Read all YAML documents in the file and unmarshal them into a slice of DeployConfig structs
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
func GetDeployConfigs(repoRoot, deployConfigBaseDir, name, customTarget, reference string) ([]*DeployConfig, error) {
	configDir := filepath.Join(repoRoot, deployConfigBaseDir)

	files, err := os.ReadDir(configDir)
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

	// Get repo and change to reference in c.Reference if it is different to the current reference in the repoRoot,
	// otherwise it will cause issues with the auto-discovery
	baseRepo, err := git.PlainOpen(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository at %s: %w", repoRoot, err)
	}

	// Compare the resolved reference with the current HEAD reference, if they are different then skip the auto-discovery for this deployment config
	headRef, err := baseRepo.Head()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", gitInternal.ErrGetHeadFailed, err)
	}

	// Checkout repo to different reference
	w, err := baseRepo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("failed to get git worktree: %w", err)
	}

	// Defer checkout back to original HEAD reference after the deployment is done
	defer func(branch plumbing.ReferenceName) {
		err = w.Checkout(&git.CheckoutOptions{
			Branch: branch,
			Keep:   true,
		})
		if err != nil {
			slog.Error("failed to checkout back to original HEAD reference after deployment", "error", err)
		}
	}(headRef.Name())

	var configs []*DeployConfig
	for _, configFile := range DeploymentConfigFileNames {
		configs, err = getDeployConfigsFromFile(configDir, files, configFile)
		if err != nil {
			if errors.Is(err, ErrConfigFileNotFound) {
				continue
			}

			return nil, err
		}

		appConfig, err := GetAppConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to get app config: %w", err)
		}

		// Build a new slice to avoid modifying the slice we're iterating over
		var expandedConfigs []*DeployConfig

		// Handle autodiscover deployment configs
		for _, c := range configs {
			if c.Reference == "" {
				// If the reference is not already set in the deployment config file, set it to the current reference
				c.Reference = reference
			}

			repoDir := repoRoot
			// Check for deployConfigs with AutoDiscover enabled, if true then remove this config and add new configs based on discovered compose files
			if c.AutoDiscover {
				if c.RepositoryUrl != "" {
					auth, err := gitInternal.GetAuthMethod(string(c.RepositoryUrl), appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase, appConfig.GitAccessToken)
					if err != nil {
						return nil, fmt.Errorf("failed to get auth method: %w", err)
					}

					repoDir = path.Join(path.Dir(repoRoot), gitInternal.GetRepoName(string(c.RepositoryUrl)))

					// Clone the repository to repoDir if it does not exist, otherwise fetch the latest changes and checkout to the correct reference
					_, err = gitInternal.CloneRepository(repoDir, string(c.RepositoryUrl), c.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy, auth, appConfig.GitCloneSubmodules)
					if err != nil {
						if errors.Is(err, git.ErrRepositoryAlreadyExists) {
							_, err = gitInternal.UpdateRepository(repoDir, string(c.RepositoryUrl), c.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy, auth, appConfig.GitCloneSubmodules)
							if err != nil {
								return nil, fmt.Errorf("failed to update repository: %w", err)
							}
						} else {
							return nil, fmt.Errorf("failed to clone repository: %w", err)
						}
					}
				} else {
					unlock := gitInternal.AcquirePathLock(repoRoot)
					err = gitInternal.CheckoutRepository(baseRepo, c.Reference)

					unlock()

					if err != nil {
						return nil, fmt.Errorf("failed to checkout repository to reference %s: %w", c.Reference, err)
					}
				}

				discoveredConfigs, err := autoDiscoverDeployments(repoDir, c)
				if err != nil {
					return nil, fmt.Errorf("failed to auto-discover deployment configurations: %w", err)
				}

				// Add the discovered configs to the expanded list
				expandedConfigs = append(expandedConfigs, discoveredConfigs...)
			} else {
				// Keep non-autodiscover configs as-is
				expandedConfigs = append(expandedConfigs, c)
			}
		}

		if expandedConfigs != nil {
			if err = validator.Validate(expandedConfigs); err != nil {
				return nil, err
			}

			// Check if the stack/project names are not unique
			err = validateUniqueProjectNames(expandedConfigs)
			if err != nil {
				return nil, err
			}

			return expandedConfigs, nil
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

// ResolveDeployConfigs returns deployment configs for a poll run, preferring inline
// deployments defined on the PollConfig when provided. Falls back to repository
// configuration files or default values when no inline deployments are present.
// repoRoot is the absolute path to the repository root.
// deployConfigBaseDir is the relative path from repo root where config files are located.
func ResolveDeployConfigs(poll PollConfig, repoRoot, deployConfigBaseDir, name string) ([]*DeployConfig, error) {
	// Prefer inline deployments when present
	if len(poll.Deployments) > 0 {
		configs, err := expandInlineAutoDiscoverConfigs(repoRoot, poll.Deployments)
		if err != nil {
			return nil, err
		}

		return configs, nil
	}

	// No inline deployments, use repository config discovery
	return GetDeployConfigs(repoRoot, deployConfigBaseDir, name, poll.CustomTarget, poll.Reference)
}

// expandInlineAutoDiscoverConfigs replaces inline deployments that have auto-discovery
// enabled with the discovered deployments rooted at repoRoot.
func expandInlineAutoDiscoverConfigs(repoRoot string, deployments []*DeployConfig) ([]*DeployConfig, error) {
	expanded := make([]*DeployConfig, 0, len(deployments))

	for _, deployment := range deployments {
		if !deployment.AutoDiscover {
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
// and generates DeployConfig entries for each.
// repoRoot is the absolute path to the repository root.
// baseDeployConfig.WorkingDirectory is treated as repo-root-relative.
func autoDiscoverDeployments(repoRoot string, baseDeployConfig *DeployConfig) ([]*DeployConfig, error) {
	var configs []*DeployConfig

	searchPath := filepath.Join(repoRoot, baseDeployConfig.WorkingDirectory)

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
		if d.IsDir() && depth > baseDeployConfig.AutoDiscoverOpts.ScanDepth && baseDeployConfig.AutoDiscoverOpts.ScanDepth > 0 {
			return filepath.SkipDir
		}

		if !d.IsDir() {
			return nil
		}

		// Check if the directory contains any docker-compose files
		for _, composeFile := range baseDeployConfig.ComposeFiles {
			composeFilePath := filepath.Join(p, composeFile)
			if _, err = os.Stat(composeFilePath); err == nil {
				c := &DeployConfig{}
				deepCopy(baseDeployConfig, c)

				stackDirName := filepath.Base(p)    // Get the stack name from the directory name where the compose file is located
				repoName := filepath.Base(repoRoot) // Get the repository name from the repo root path

				if baseDeployConfig.Name != "" && stackDirName == repoName {
					c.Name = baseDeployConfig.Name
				} else {
					c.Name = stackDirName
				}

				c.WorkingDirectory, err = filepath.Rel(repoRoot, p)
				if err != nil {
					return err
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

// deepCopy creates a deep copy of a DeployConfig struct.
func deepCopy(src, dst *DeployConfig) {
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
		for k, v := range src.BuildOpts.Args {
			dst.BuildOpts.Args[k] = v
		}
	}

	if src.Profiles != nil {
		dst.Profiles = make([]string, len(src.Profiles))
		copy(dst.Profiles, src.Profiles)
	}

	if src.ExternalSecrets != nil {
		dst.ExternalSecrets = make(map[string]string)
		for k, v := range src.ExternalSecrets {
			dst.ExternalSecrets[k] = v
		}
	}
}
