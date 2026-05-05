package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
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
	ErrMultipleYAMLDocuments         = errors.New("nested .doco-cd configuration file must contain only a single YAML document")
	supportedReconciliationEvents    = map[string]struct{}{
		"die":       {},
		"destroy":   {},
		"update":    {},
		"stop":      {},
		"kill":      {},
		"oom":       {},
		"unhealthy": {},
	}
)

const DefaultReference = "refs/heads/main"

// AutoDiscoveryConfig holds auto-discovery settings for a deployment.
type AutoDiscoveryConfig struct {
	Enable    bool `yaml:"enable" json:"enable" default:"false"` // Enable enables autodiscovery of services to deploy in the working directory
	ScanDepth int  `yaml:"depth" json:"depth" default:"0"`       // ScanDepth is the maximum depth of subdirectories to scan for docker-compose files
	Delete    bool `yaml:"delete" json:"delete" default:"true"`  // Delete removes obsolete auto-discovered deployments that are no longer present in the repository
}

// BuildConfig holds build options for a deployment.
type BuildConfig struct {
	ForceImagePull bool              `yaml:"force_image_pull" json:"force_image_pull" default:"false"` // ForceImagePull always attempt to pull a newer version of the image
	Quiet          bool              `yaml:"quiet" json:"quiet" default:"false"`                       // Quiet suppresses the build output
	Args           map[string]string `yaml:"args" json:"args"`                                         // BuildArgs is a map of build-time arguments to pass to the build process
	NoCache        bool              `yaml:"no_cache" json:"no_cache" default:"false"`                 // NoCache disables the use of the cache when building images
}

// DestroyConfig holds options for destroying a deployment.
type DestroyConfig struct {
	Enable        bool `yaml:"enable" json:"enable" default:"false"`                // Enable removes the deployment and all its resources from the Docker host
	RemoveVolumes bool `yaml:"remove_volumes" json:"remove_volumes" default:"true"` // RemoveVolumes removes the volumes used by the deployment (always enabled in docker swarm mode)
	RemoveImages  bool `yaml:"remove_images" json:"remove_images" default:"true"`   // RemoveImages removes the images used by the deployment (currently not supported in docker swarm mode)
	RemoveRepoDir bool `yaml:"remove_dir" json:"remove_dir" default:"true"`         // RemoveRepoDir removes the repository directory after the deployment is destroyed
}

// ReconciliationConfig holds settings for the reconciliation feature.
type ReconciliationConfig struct {
	Enabled        bool     `yaml:"enabled" json:"enabled" default:"true"`               // Enabled enables the reconciliation feature
	Events         []string `yaml:"events" json:"events" default:"[\"unhealthy\"]"`      // Events is the list of Docker container actions that trigger reconciliation
	RestartTimeout int      `yaml:"restart_timeout" json:"restart_timeout" default:"10"` // RestartTimeout is the timeout in seconds to wait before killing a container during a restart
	RestartSignal  string   `yaml:"restart_signal" json:"restart_signal" default:""`     // RestartSignal is the signal sent to stop containers during a restart. If not set, the default of the Docker daemon is used (SIGTERM).
	RestartLimit   int      `yaml:"restart_limit" json:"restart_limit" default:"5"`      // RestartLimit suppresses further unhealthy-triggered restarts after this many restarts in the configured window. Set to 0 to disable suppression.
	RestartWindow  int      `yaml:"restart_window" json:"restart_window" default:"300"`  // RestartWindow is the time window in seconds used with RestartLimit.
}

// DeployConfig is the structure of the deployment configuration file.
type DeployConfig struct {
	Name               string                       `yaml:"name" json:"name" doco:"allowOverride"`                                                                                                                  // Name of the docker-compose deployment / stack
	RepositoryUrl      HttpUrl                      `yaml:"repository_url" json:"repository_url" default:"" validate:"httpUrl"`                                                                                     // RepositoryUrl is the http URL of the Git repository to deploy
	WebhookEventFilter string                       `yaml:"webhook_filter" json:"webhook_filter" default:"" doco:"allowOverride"`                                                                                   // WebhookEventFilter is a regular expression to whitelist deployment triggers based on the webhook event payload (e.g., branch like "^refs/heads/main$" or "main", tag like "^refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$" or "v[0-9]+\.[0-9]+\.[0-9]+")
	Reference          string                       `yaml:"reference" json:"reference" default:""`                                                                                                                  // Reference is the Git reference to the deployment, e.g., refs/heads/main, main, refs/tags/v1.0.0 or v1.0.0
	WorkingDirectory   string                       `yaml:"working_dir" json:"working_dir" default:"." doco:"allowOverride"`                                                                                        // WorkingDirectory is the working directory for the deployment
	ComposeFiles       []string                     `yaml:"compose_files" json:"compose_files" default:"[\"compose.yaml\", \"compose.yml\", \"docker-compose.yml\", \"docker-compose.yaml\"]" doco:"allowOverride"` // ComposeFiles is the list of docker-compose files to use
	Environment        map[string]string            `yaml:"environment" json:"environment" doco:"allowOverride"`                                                                                                    // Environment is a map of environment variables to use for variable interpolation in the compose files
	EnvFiles           []string                     `yaml:"env_files" json:"env_files" default:"[\".env\"]" doco:"allowOverride"`                                                                                   // EnvFiles is the list of dotenv files to use for variable interpolation
	RemoveOrphans      bool                         `yaml:"remove_orphans" json:"remove_orphans" default:"true" doco:"allowOverride"`                                                                               // RemoveOrphans removes containers for services not defined in the Compose file
	PruneImages        bool                         `yaml:"prune_images" json:"prune_images" default:"true" doco:"allowOverride"`                                                                                   // PruneImages removes images that are no longer used by any service
	ForceRecreate      bool                         `yaml:"force_recreate" json:"force_recreate" default:"false" doco:"allowOverride"`                                                                              // ForceRecreate forces the recreation/redeployment of containers even if the configuration has not changed
	ForceImagePull     bool                         `yaml:"force_image_pull" json:"force_image_pull" default:"false" doco:"allowOverride"`                                                                          // ForceImagePull always pulls the latest version of the image tags you've specified if a newer version is available
	Timeout            int                          `yaml:"timeout" json:"timeout" default:"180" doco:"allowOverride"`                                                                                              // Timeout is the time in seconds to wait for the deployment to finish before timing out
	BuildOpts          BuildConfig                  `yaml:"build_opts" doco:"allowOverride"`                                                                                                                        // BuildOpts is the build options for the deployment
	GitDepth           int                          `yaml:"git_depth" json:"git_depth" default:"0"`                                                                                                                 // GitDepth limits the number of commits to fetch. 0 means use global GIT_CLONE_DEPTH. A positive value overrides the global setting.
	Destroy            DestroyConfig                `yaml:"destroy" json:"destroy" doco:"allowOverride"`                                                                                                            // Destroy configures destruction of the deployment and related resources
	Profiles           []string                     `yaml:"profiles" json:"profiles" default:"[]" doco:"allowOverride"`                                                                                             // Profiles is a list of profiles to use for the deployment, e.g., ["dev", "prod"]. See https://docs.docker.com/compose/how-tos/profiles/
	ExternalSecrets    map[string]ExternalSecretRef `yaml:"external_secrets" json:"external_secrets" doco:"allowOverride"`                                                                                          // ExternalSecrets maps env vars to legacy string references or structured references (e.g. webhook store_ref/remote_ref).
	AutoDiscovery      AutoDiscoveryConfig          `yaml:"auto_discovery" json:"auto_discovery"`                                                                                                                   // AutoDiscovery configures autodiscovery of services to deploy in the working directory
	Reconciliation     ReconciliationConfig         `yaml:"reconciliation" json:"reconciliation" doco:"allowOverride"`                                                                                              // Reconciliation is the configuration for the reconciliation feature
	Internal           struct {
		File        string            `yaml:"-"` // File is the path to the deployment configuration file
		Environment map[string]string // Environment stores environment variables for variable interpolation in the compose project
		Hash        string            `yaml:"-"` // Hash is a hash of the DeployConfig struct
	} // Internal holds internal configuration values that are not set by the user
}

// ResolveGitDepth returns the effective git clone depth.
// If the deploy-level GitDepth is > 0, it overrides the global value.
// Otherwise the global depth is used. 0 means full clone (no limit).
func (c *DeployConfig) ResolveGitDepth(globalDepth int) int {
	if c.GitDepth > 0 {
		return c.GitDepth
	}

	return globalDepth
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
func (c *DeployConfig) LogValue() slog.Value {
	return logger.BuildLogValue(c, "Internal")
}

func (c *DeployConfig) validateConfig() error {
	if c.Name == "" && !c.AutoDiscovery.Enable {
		return fmt.Errorf("%w: name", ErrKeyNotFound)
	}

	if c.GitDepth < 0 {
		return fmt.Errorf("%w: git_depth must be >= 0", ErrInvalidConfig)
	}

	if c.Reconciliation.RestartTimeout < 0 {
		return fmt.Errorf("%w: reconciliation.restart_timeout must be >= 0", ErrInvalidConfig)
	}

	c.Reconciliation.RestartSignal = strings.ToUpper(strings.TrimSpace(c.Reconciliation.RestartSignal))

	if c.Reconciliation.RestartLimit < 0 {
		return fmt.Errorf("%w: reconciliation.restart_limit must be >= 0", ErrInvalidConfig)
	}

	if c.Reconciliation.RestartWindow < 0 {
		return fmt.Errorf("%w: reconciliation.restart_window must be >= 0", ErrInvalidConfig)
	}

	if c.Reconciliation.RestartLimit > 0 && c.Reconciliation.RestartWindow == 0 {
		return fmt.Errorf("%w: reconciliation.restart_window must be > 0 when reconciliation.restart_limit is set", ErrInvalidConfig)
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

	if err := c.normalizeReconciliationEvents(); err != nil {
		return err
	}

	return nil
}

func (c *DeployConfig) normalizeReconciliationEvents() error {
	if len(c.Reconciliation.Events) == 0 {
		c.Reconciliation.Enabled = false
		return nil
	}

	normalized := make([]string, 0, len(c.Reconciliation.Events))
	seen := make(map[string]struct{}, len(c.Reconciliation.Events))

	for _, rawEvent := range c.Reconciliation.Events {
		event := strings.ToLower(strings.TrimSpace(rawEvent))

		switch event {
		case "remove", "delete":
			event = "destroy"
		}

		if event == "" {
			return fmt.Errorf("%w: reconciliation.events contains an empty event", ErrInvalidConfig)
		}

		if _, ok := supportedReconciliationEvents[event]; !ok {
			return fmt.Errorf("%w: unsupported reconciliation event %q", ErrInvalidConfig, rawEvent)
		}

		if _, exists := seen[event]; exists {
			continue
		}

		seen[event] = struct{}{}
		normalized = append(normalized, event)
	}

	c.Reconciliation.Events = normalized

	return nil
}

func (c *DeployConfig) UnmarshalYAML(unmarshal func(any) error) error {
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
// When applyDefaults is true, default values are applied to each config (normal usage).
// When applyDefaults is false, omitted fields remain zero/nil — used for nested auto-discovery
// overrides so unset fields do not accidentally replace base/discovered values during merge.
func GetDeployConfigFromYAML(f string, applyDefaults bool) ([]*DeployConfig, error) {
	b, err := os.ReadFile(f) // #nosec G304
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %v", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(b))

	var configs []*DeployConfig

	// Use a type alias to bypass the UnmarshalYAML hook (which injects defaults)
	// when the caller explicitly does not want defaults applied.
	type deployConfigNoDefaults DeployConfig

	for {
		var c DeployConfig

		if applyDefaults {
			err = dec.Decode(&c)
		} else {
			var raw deployConfigNoDefaults

			err = dec.Decode(&raw)
			c = DeployConfig(raw)
		}

		if err != nil {
			if err == io.EOF {
				break
			}

			return nil, fmt.Errorf("failed to decode yaml: %v", err)
		}

		c.Internal.File = f

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
			if c.AutoDiscovery.Enable {
				if c.RepositoryUrl != "" {
					auth, err := gitInternal.GetAuthMethod(string(c.RepositoryUrl), appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase, appConfig.GitAccessToken)
					if err != nil {
						return nil, fmt.Errorf("failed to get auth method: %w", err)
					}

					repoDir = path.Join(path.Dir(repoRoot), gitInternal.GetRepoName(string(c.RepositoryUrl)))

					// Clone the repository to repoDir if it does not exist, otherwise fetch the latest changes and checkout to the correct reference
					_, err = gitInternal.CloneRepository(repoDir, string(c.RepositoryUrl), c.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy, auth, appConfig.GitCloneSubmodules, c.ResolveGitDepth(appConfig.GitCloneDepth))
					if err != nil {
						if errors.Is(err, git.ErrRepositoryAlreadyExists) {
							_, err = gitInternal.UpdateRepository(repoDir, string(c.RepositoryUrl), c.Reference, appConfig.SkipTLSVerification, appConfig.HttpProxy, auth, appConfig.GitCloneSubmodules, c.ResolveGitDepth(appConfig.GitCloneDepth))
							if err != nil {
								return nil, fmt.Errorf("failed to update repository: %w", err)
							}
						} else {
							return nil, fmt.Errorf("failed to clone repository: %w", err)
						}
					}
				} else {
					auth, err := gitInternal.GetAuthMethod(string(c.RepositoryUrl), appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase, appConfig.GitAccessToken)
					if err != nil {
						return nil, fmt.Errorf("failed to get auth method: %w", err)
					}

					unlock := gitInternal.AcquirePathLock(repoRoot)
					err = gitInternal.CheckoutRepository(baseRepo, c.Reference, auth, appConfig.GitCloneSubmodules)

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
		return nil, fmt.Errorf("%w: .doco-cd.%s.y(a)ml", ErrConfigFileNotFound, customTarget)
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
			configs, err := GetDeployConfigFromYAML(path.Join(dir, f.Name()), true)
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
		if d.IsDir() && depth > baseDeployConfig.AutoDiscovery.ScanDepth && baseDeployConfig.AutoDiscovery.ScanDepth > 0 {
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

				// Check for a nested .doco-cd config file alongside the compose file and
				// merge any overridable fields from it on top of the base config copy.
				for _, cfgName := range DefaultDeploymentConfigFileNames {
					localCfgPath := filepath.Join(p, cfgName)
					if _, statErr := os.Stat(localCfgPath); statErr != nil {
						continue
					}

					localConfigs, parseErr := GetDeployConfigFromYAML(localCfgPath, false)
					if parseErr != nil {
						return fmt.Errorf("failed to parse nested .doco-cd config at %s: %w", localCfgPath, parseErr)
					}

					if len(localConfigs) > 1 {
						return fmt.Errorf("%w: %s contains %d documents", ErrMultipleYAMLDocuments, localCfgPath, len(localConfigs))
					}

					mergeDeployConfig(c, localConfigs[0])

					break // use first found config file name (.yaml preferred over .yml)
				}

				if err = c.validateConfig(); err != nil {
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

// mergeDeployConfig merges fields from override into base, but only for fields
// tagged with `doco:"allowOverride"`. Protected fields (reference, repository_url,
// auto_discovery, git_depth) are never overridden.
// Merge semantics:
//   - Maps: merged key-by-key (override wins on key collision)
//   - Slices: replaced entirely if the override slice is non-empty
//   - Nested structs: all sub-fields are merged (parent tag opts them in)
//   - Scalars: replaced if the override value is non-zero
func mergeDeployConfig(base, override *DeployConfig) {
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
		maps.Copy(dst.BuildOpts.Args, src.BuildOpts.Args)
	}

	if src.Profiles != nil {
		dst.Profiles = make([]string, len(src.Profiles))
		copy(dst.Profiles, src.Profiles)
	}

	if src.ExternalSecrets != nil {
		dst.ExternalSecrets = make(map[string]ExternalSecretRef)
		maps.Copy(dst.ExternalSecrets, src.ExternalSecrets)
	}
}
