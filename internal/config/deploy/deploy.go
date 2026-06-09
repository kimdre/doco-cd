package deploy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
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

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/hook"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"

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
)

// Config is the structure of the deployment configuration file.
type Config struct {
	Name               string                                   `yaml:"name" json:"name" doco:"allowOverride"`                                                                                                                  // Name of the docker-compose deployment / stack
	Source             config.SourceType                        `yaml:"source" json:"source" default:"git"`                                                                                                                     // Source selects the deployment source backend (git or oci)
	Version            string                                   `yaml:"version" json:"version" default:"doco.v1" doco:"allowOverride"`                                                                                          // Version declares the deployment config schema/artifact version for OCI-backed deployments
	RepositoryUrl      config.GitUrl                            `yaml:"repository_url" json:"repository_url" default:"" validate:"gitUrl"`                                                                                      // RepositoryUrl is the Git clone URL of the repository to deploy
	WebhookEventFilter string                                   `yaml:"webhook_filter" json:"webhook_filter" default:"" doco:"allowOverride"`                                                                                   // WebhookEventFilter is a regular expression to whitelist deployment triggers based on the webhook event payload (e.g., branch like "^refs/heads/main$" or "main", tag like "^refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$" or "v[0-9]+\.[0-9]+\.[0-9]+")
	Reference          string                                   `yaml:"reference" json:"reference" default:""`                                                                                                                  // Reference is the Git reference to the deployment, e.g., refs/heads/main, main, refs/tags/v1.0.0 or v1.0.0
	WorkingDirectory   string                                   `yaml:"working_dir" json:"working_dir" default:"." doco:"allowOverride"`                                                                                        // WorkingDirectory is the working directory for the deployment
	ComposeFiles       []string                                 `yaml:"compose_files" json:"compose_files" default:"[\"compose.yaml\", \"compose.yml\", \"docker-compose.yml\", \"docker-compose.yaml\"]" doco:"allowOverride"` // ComposeFiles is the list of docker-compose files to use
	Environment        map[string]string                        `yaml:"environment" json:"environment" doco:"allowOverride"`                                                                                                    // Environment is a map of environment variables to use for variable interpolation in the compose files
	EnvFiles           []string                                 `yaml:"env_files" json:"env_files" default:"[\".env\"]" doco:"allowOverride"`                                                                                   // EnvFiles is the list of dotenv files to use for variable interpolation
	RemoveOrphans      bool                                     `yaml:"remove_orphans" json:"remove_orphans" default:"true" doco:"allowOverride"`                                                                               // RemoveOrphans removes containers for services not defined in the Compose file
	PruneImages        bool                                     `yaml:"prune_images" json:"prune_images" default:"true" doco:"allowOverride"`                                                                                   // PruneImages removes images that are no longer used by any service
	WaitRunningJobs    bool                                     `yaml:"wait_running_jobs" json:"wait_running_jobs" default:"true" doco:"allowOverride"`                                                                         // WaitRunningJobs waits for currently running scheduled job containers/services to finish before deployment
	ForceRecreate      bool                                     `yaml:"force_recreate" json:"force_recreate" default:"false" doco:"allowOverride"`                                                                              // ForceRecreate forces the recreation/redeployment of containers even if the configuration has not changed
	ForceImagePull     bool                                     `yaml:"force_image_pull" json:"force_image_pull" default:"false" doco:"allowOverride"`                                                                          // ForceImagePull always pulls the latest version of the image tags you've specified if a newer version is available
	Timeout            int                                      `yaml:"timeout" json:"timeout" default:"180" doco:"allowOverride"`                                                                                              // Timeout is the time in seconds to wait for the deployment to finish before timing out
	BuildOpts          BuildConfig                              `yaml:"build" json:"build" doco:"allowOverride"`                                                                                                                // BuildOpts is the build options for the deployment
	GitDepth           int                                      `yaml:"git_depth" json:"git_depth" default:"0"`                                                                                                                 // GitDepth limits the number of commits to fetch. 0 means use global GIT_CLONE_DEPTH. A positive value overrides the global setting.
	Destroy            DestroyConfig                            `yaml:"destroy" json:"destroy" doco:"allowOverride"`                                                                                                            // Destroy configures destruction of the deployment and related resources
	Profiles           []string                                 `yaml:"profiles" json:"profiles" default:"[]" doco:"allowOverride"`                                                                                             // Profiles is a list of profiles to use for the deployment, e.g., ["dev", "prod"]. See https://docs.docker.com/compose/how-tos/profiles/
	ExternalSecrets    map[string]secrettypes.ExternalSecretRef `yaml:"external_secrets" json:"external_secrets" doco:"allowOverride"`                                                                                          // ExternalSecrets maps env vars to legacy string references or structured references (e.g. webhook store_ref/remote_ref).
	AutoDiscovery      AutoDiscoveryConfig                      `yaml:"auto_discovery" json:"auto_discovery"`                                                                                                                   // AutoDiscovery configures autodiscovery of services to deploy in the working directory
	Reconciliation     ReconciliationConfig                     `yaml:"reconciliation" json:"reconciliation" doco:"allowOverride"`                                                                                              // Reconciliation is the configuration for the reconciliation feature
	Oci                config.OciTrustPolicyOverride            `yaml:"oci" json:"oci" doco:"allowOverride"`                                                                                                                    // Oci allows per-target overrides for OCI signature verification policy
	Hooks              hook.Config                              `yaml:"hooks" json:"hooks" doco:"allowOverride"`                                                                                                                // Hooks configures HTTP webhook hooks fired on deployment success/failure
	Internal           struct {
		File                          string            `yaml:"-"` // File is the path to the deployment configuration file
		Environment                   map[string]string // Environment stores environment variables for variable interpolation in the compose project
		Hash                          string            `yaml:"-"`          // Hash is a hash of the Config struct
		OciTrustPolicyOverrideTrusted bool              `yaml:"-" json:"-"` // true only for trusted config sources (e.g. POLL_CONFIG inline deployments)
	} // Internal holds internal configuration values that are not set by the user
}

// ResolveGitDepth returns the effective git clone depth.
// If the deploy-level GitDepth is > 0, it overrides the global value.
// Otherwise, the global depth is used. 0 means full clone (no limit).
func (c *Config) ResolveGitDepth(globalDepth int) int {
	if c.GitDepth > 0 {
		return c.GitDepth
	}

	return globalDepth
}

// New creates a Config with default values.
func New(name, reference string) *Config {
	return &Config{
		Name:             name,
		Reference:        reference,
		WorkingDirectory: ".",
		ComposeFiles:     cli.DefaultFileNames,
	}
}

// LogValue implements the slog.LogValuer interface for Config.
func (c *Config) LogValue() slog.Value {
	return logger.BuildLogValue(c, "Internal")
}

func (c *Config) Validate() error {
	c.Source = config.NormalizeSourceType(c.Source)
	if err := config.ValidateSourceType(c.Source); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	c.Version = strings.TrimSpace(c.Version)

	if c.Version == "" {
		c.Version = config.OciArtifactLayoutV1
	}

	if c.Source == config.SourceTypeOCI && c.Version != config.OciArtifactLayoutV1 {
		return fmt.Errorf("%w: unsupported oci version %q", ErrInvalidConfig, c.Version)
	}

	if c.Name == "" && !c.AutoDiscovery.Enabled {
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

	if err := c.Hooks.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	return nil
}

func (c *Config) UnmarshalYAML(unmarshal func(any) error) error {
	err := defaults.Set(c)
	if err != nil {
		return err
	}

	type Plain Config

	if err := unmarshal((*Plain)(c)); err != nil {
		return err
	}

	return nil
}

func (c *Config) UnmarshalJSON(data []byte) error {
	err := defaults.Set(c)
	if err != nil {
		return err
	}

	type Plain Config

	if err := json.Unmarshal(data, (*Plain)(c)); err != nil {
		return err
	}

	return nil
}

// Hash returns a hash of the Config struct (without changing the order of its elements).
func (c *Config) Hash() (string, error) {
	data, err := yaml.Marshal(c)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// GetConfigFromYAML reads a YAML file and unmarshals it into a slice of Config structs.
// When applyDefaults is true, default values are applied to each config (normal usage).
// When applyDefaults is false, omitted fields remain zero/nil — used for nested auto-discovery
// overrides so unset fields do not accidentally replace base/discovered values during merge.
func GetConfigFromYAML(f string, applyDefaults bool) ([]*Config, error) {
	b, err := os.ReadFile(f) // #nosec G304
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %v", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(b))

	var configs []*Config

	// Use a type alias to bypass the UnmarshalYAML hook (which injects defaults)
	// when the caller explicitly does not want defaults applied.
	type configNoDefaults Config

	for {
		var c Config

		if applyDefaults {
			err = dec.Decode(&c)
		} else {
			var raw configNoDefaults

			err = dec.Decode(&raw)
			c = Config(raw)
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

// GetConfigs returns deployment configurations discovered in the repository.
// It fails when no matching deployment configuration file exists.
// gitOpts is optional (can be nil) and is only required when AutoDiscovery with a remote RepositoryUrl is used.
func GetConfigs(repoRoot, configBaseDir, customTarget, reference string, gitOpts *GitOptions) ([]*Config, error) {
	configDir := filepath.Join(repoRoot, configBaseDir)

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
	// otherwise it will cause issues with the auto-discovery.
	// For non-git sources (e.g. OCI), the directory is not a git repository, so we skip git operations.
	baseRepo, err := git.PlainOpen(repoRoot)
	isGitRepo := true

	if err != nil {
		if !errors.Is(err, git.ErrRepositoryNotExists) {
			return nil, fmt.Errorf("failed to open git repository at %s: %w", repoRoot, err)
		}

		isGitRepo = false
	}

	if isGitRepo {
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
	}

	var configs []*Config
	for _, configFile := range DeploymentConfigFileNames {
		configs, err = getConfigsFromFile(configDir, files, configFile)
		if err != nil {
			if errors.Is(err, ErrConfigFileNotFound) {
				continue
			}

			return nil, err
		}

		// Build a new slice to avoid modifying the slice we're iterating over
		var expandedConfigs []*Config

		// Ensure gitOpts is not nil for AutoDiscovery operations
		opts := gitOpts
		if opts == nil {
			opts = &GitOptions{}
		}

		// Handle autodiscover deployment configs
		for _, c := range configs {
			if c.Reference == "" {
				// If the reference is not already set in the deployment config file, set it to the current reference
				c.Reference = reference
			}

			repoDir := repoRoot
			// Check for configs with AutoDiscover enabled, if true then remove this config and add new configs based on discovered compose files
			if c.AutoDiscovery.Enabled {
				if c.RepositoryUrl != "" {
					auth, err := gitInternal.GetAuthMethod(string(c.RepositoryUrl), opts.SSHPrivateKey, opts.SSHPrivateKeyPassphrase, opts.GitAccessToken)
					if err != nil {
						return nil, fmt.Errorf("failed to get auth method: %w", err)
					}

					repoDir = path.Join(path.Dir(repoRoot), gitInternal.GetRepoName(string(c.RepositoryUrl)))

					// Clone the repository to repoDir if it does not exist, otherwise fetch the latest changes and checkout to the correct reference
					_, err = gitInternal.CloneRepository(repoDir, string(c.RepositoryUrl), c.Reference, opts.SkipTLSVerification, opts.HttpProxy, auth, opts.GitCloneSubmodules, c.ResolveGitDepth(opts.GitCloneDepth))
					if err != nil {
						if errors.Is(err, git.ErrRepositoryAlreadyExists) {
							_, err = gitInternal.UpdateRepository(repoDir, string(c.RepositoryUrl), c.Reference, opts.SkipTLSVerification, opts.HttpProxy, auth, opts.GitCloneSubmodules, c.ResolveGitDepth(opts.GitCloneDepth))
							if err != nil {
								return nil, fmt.Errorf("failed to update repository: %w", err)
							}
						} else {
							return nil, fmt.Errorf("failed to clone repository: %w", err)
						}
					}
				} else if isGitRepo {
					auth, err := gitInternal.GetAuthMethod(string(c.RepositoryUrl), opts.SSHPrivateKey, opts.SSHPrivateKeyPassphrase, opts.GitAccessToken)
					if err != nil {
						return nil, fmt.Errorf("failed to get auth method: %w", err)
					}

					unlock := gitInternal.AcquirePathLock(repoRoot)
					err = gitInternal.CheckoutRepository(baseRepo, c.Reference, auth, opts.GitCloneSubmodules)

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
			err = ValidateUniqueProjectNames(expandedConfigs)
			if err != nil {
				return nil, err
			}

			return expandedConfigs, nil
		}
	}

	if customTarget != "" {
		return nil, fmt.Errorf("%w: .doco-cd.%s.y(a)ml", ErrConfigFileNotFound, customTarget)
	}

	return nil, fmt.Errorf("%w: .doco-cd.y(a)ml", ErrConfigFileNotFound)
}

// getConfigsFromFile returns the deployment configurations from the repository or nil if not found.
func getConfigsFromFile(dir string, files []os.DirEntry, configFile string) ([]*Config, error) {
	for _, f := range files {
		if f.IsDir() {
			continue
		}

		if f.Name() == configFile {
			// Get contents of deploy config file
			configs, err := GetConfigFromYAML(path.Join(dir, f.Name()), true)
			if err != nil {
				return nil, err
			}

			// Validate all deploy configs
			for _, c := range configs {
				if err = c.Validate(); err != nil {
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

// ValidateUniqueProjectNames checks if the project names in the configs are unique.
func ValidateUniqueProjectNames(configs []*Config) error {
	names := make(map[string]bool)
	for _, dc := range configs {
		if names[dc.Name] {
			return fmt.Errorf("%w: %s", ErrDuplicateProjectName, dc.Name)
		}

		names[dc.Name] = true
	}

	return nil
}

// ResolveConfigs returns Deployment Config's for a poll run, preferring inline
// deployments defined on the PollConfig when provided. Inline deployments bypass
// repository config file discovery. When no inline deployments are present,
// repository config files are required.
// repoRoot is the absolute path to the repository root.
// configBaseDir is the relative path from repo root where config files are located.
// gitOpts is optional (may be nil) and is only required when AutoDiscovery with a remote RepositoryUrl is used.
func ResolveConfigs(inlineDeployments []*Config, customTarget, reference, repoRoot, configBaseDir string, gitOpts *GitOptions) ([]*Config, error) {
	// Prefer inline deployments when present
	if len(inlineDeployments) > 0 {
		// Apply reference to inline deployments if not already set
		for _, d := range inlineDeployments {
			if d.Reference == "" {
				d.Reference = reference
			}
		}

		configs, err := expandInlineAutoDiscoverConfigs(repoRoot, inlineDeployments)
		if err != nil {
			return nil, err
		}

		for _, cfg := range configs {
			cfg.Internal.OciTrustPolicyOverrideTrusted = true
		}

		return configs, nil
	}

	// No inline deployments, use repository config discovery
	return GetConfigs(repoRoot, configBaseDir, customTarget, reference, gitOpts)
}
