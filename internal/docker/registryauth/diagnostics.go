package registryauth

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	distReference "github.com/distribution/reference"
	"github.com/docker/cli/cli/config/configfile"

	"github.com/kimdre/doco-cd/internal/common/types/set"
)

const (
	dockerHubDomain       = "docker.io"
	dockerHubIndexDomain  = "index.docker.io"
	dockerHubRegistryHost = "registry-1.docker.io"
	dockerHubAuthConfig   = "https://index.docker.io/v1/"
)

// CheckDockerConfigReadable verifies if the docker config file is readable
// for the current user in the container. It checks the path specified by the
// DOCKER_CONFIG environment variable or defaults to ~/.docker/config.json.
// Returns an error if the config file exists but is not readable or contains invalid content.
// Also checks for missing credential helper binaries configured in the docker config.
func CheckDockerConfigReadable(cfg *configfile.ConfigFile) error {
	if cfg == nil {
		return errors.New("docker config is nil")
	}

	// Determine the config path to check
	configPath := cfg.Filename
	if configPath == "" {
		// If no config file is loaded, use the DOCKER_CONFIG env var or default path
		dockerConfigEnv := strings.TrimSpace(os.Getenv("DOCKER_CONFIG"))
		if dockerConfigEnv != "" {
			configPath = filepath.Join(dockerConfigEnv, "config.json")
		} else {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("failed to determine home directory: %w", err)
			}

			configPath = filepath.Join(homeDir, ".docker", "config.json")
		}
	}

	configPath = filepath.Clean(configPath)

	// Check if the config file exists
	fileInfo, err := os.Stat(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Config file doesn't exist, which is acceptable
			return nil
		}

		return fmt.Errorf("failed to check docker config file %q: %w", configPath, err)
	}

	// If it exists, verify it's a regular file
	if !fileInfo.Mode().IsRegular() {
		return fmt.Errorf("docker config path %q is not a regular file", configPath)
	}

	// Verify the file is readable and contains valid content using docker's own loader
	if err := validateDockerConfigContent(configPath); err != nil {
		return err
	}

	// Check for missing credential helper binaries configured in docker config
	if err := checkMissingCredentialHelpers(cfg); err != nil {
		return err
	}

	return nil
}

// validateDockerConfigContent checks if the docker config file is readable and contains valid content.
func validateDockerConfigContent(configPath string) error {
	file, err := os.Open(configPath)
	if err != nil {
		return fmt.Errorf("docker config file %q is not readable: %w", configPath, err)
	}
	defer file.Close()

	testCfg := configfile.New(configPath)
	if err := testCfg.LoadFromReader(file); err != nil {
		return fmt.Errorf("docker config file %q is invalid: %w", configPath, err)
	}

	return nil
}

// checkMissingCredentialHelpers verifies that all credential helpers configured in the docker config
// are available in the system's PATH. Returns an error with details about missing helpers.
func checkMissingCredentialHelpers(cfg *configfile.ConfigFile) error {
	if cfg == nil {
		return nil
	}

	// If no credential helpers are configured, there's nothing to check
	if cfg.CredentialsStore == "" && len(cfg.CredentialHelpers) == 0 {
		return nil
	}

	missing := missingConfiguredCredentialHelpers(cfg)
	if len(missing) > 0 {
		return fmt.Errorf("missing credential helper binaries in container: %s", strings.Join(missing, ", "))
	}

	return nil
}

// IsAuthRelatedError reports whether the provided error looks like a
// registry-authorization/authentication failure.
func IsAuthRelatedError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	authSignals := []string{
		"access denied",
		"access forbidden",
		"authentication required",
		"authorization",
		"error getting credentials",
		"insufficient_scope",
		"no basic auth credentials",
		"pull access denied",
		"unauthorized",
	}

	for _, signal := range authSignals {
		if strings.Contains(msg, signal) {
			return true
		}
	}

	return false
}

// WrapLookupError wraps Docker auth token lookup failures with actionable
// hints for private-registry setups.
func WrapLookupError(cfg *configfile.ConfigFile, imageRef string, lookupErr error) error {
	if lookupErr == nil {
		return nil
	}

	hint := BuildFailureHint(cfg, []string{imageRef}, lookupErr)
	if hint == "" {
		return fmt.Errorf("retrieve auth token from image %q: %w", imageRef, lookupErr)
	}

	return fmt.Errorf("retrieve auth token from image %q: %w; %s", imageRef, lookupErr, hint)
}

// BuildFailureHint returns user-facing guidance for registry auth failures.
// It returns an empty string for non-auth-related errors.
func BuildFailureHint(cfg *configfile.ConfigFile, imageRefs []string, failure error) string {
	if cfg == nil || !IsAuthRelatedError(failure) {
		return ""
	}

	registries := imageRegistries(imageRefs)
	if len(registries) == 0 {
		return ""
	}

	var hints []string

	if cfg.Filename != "" {
		hints = append(hints, "docker config path: "+cfg.Filename)
	}

	if dockerConfig := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); dockerConfig != "" {
		hints = append(hints, "DOCKER_CONFIG="+dockerConfig)
	}

	missingHelpers := missingCredentialHelpers(cfg, registries)
	if len(missingHelpers) > 0 {
		hints = append(hints, "missing helper binaries in container: "+strings.Join(missingHelpers, ", "))
	}

	missingAuth := registriesWithoutConfigAuth(cfg, registries)
	if len(missingAuth) > 0 {
		hints = append(hints, "no credentials configured for registries: "+strings.Join(missingAuth, ", "))
	}

	if (cfg.CredentialsStore != "" || len(cfg.CredentialHelpers) > 0) && len(cfg.AuthConfigs) == 0 {
		hints = append(hints, "helper-based credentials require matching docker-credential-* binaries inside the doco-cd container")
	}

	if len(hints) == 0 {
		return ""
	}

	return "registry auth hint: " + strings.Join(hints, "; ")
}

// imageRegistries returns a sorted list of unique registry domains extracted from the provided image references.
func imageRegistries(imageRefs []string) []string {
	unique := set.Set[string]{}

	for _, ref := range imageRefs {
		namedRef, err := distReference.ParseNormalizedNamed(ref)
		if err != nil {
			continue
		}

		registry := distReference.Domain(namedRef)
		if registry == "" {
			continue
		}

		unique.Add(registry)
	}

	registries := make([]string, 0, len(unique))
	for registry := range unique {
		registries = append(registries, registry)
	}

	sort.Strings(registries)

	return registries
}

// registriesWithoutConfigAuth returns a list of registries for which the provided Docker config file
// does not contain any authentication information (neither inline auth nor configured credential helpers).
func registriesWithoutConfigAuth(cfg *configfile.ConfigFile, registries []string) []string {
	var missing []string

	for _, registry := range registries {
		if hasInlineAuth(cfg, registry) || configuredHelper(cfg, registry) != "" {
			continue
		}

		missing = append(missing, registry)
	}

	return missing
}

// missingCredentialHelpers returns a list of credential helpers that are configured
// in the Docker config file but are not found in the system's PATH.
func missingCredentialHelpers(cfg *configfile.ConfigFile, registries []string) []string {
	helpers := make([]string, 0, len(registries))

	for _, registry := range registries {
		helpers = append(helpers, configuredHelper(cfg, registry))
	}

	return missingCredentialHelperBinaries(helpers)
}

// missingConfiguredCredentialHelpers returns missing helpers explicitly configured
// by the Docker config, including both the global store and registry-specific helpers.
func missingConfiguredCredentialHelpers(cfg *configfile.ConfigFile) []string {
	helpers := make([]string, 0, len(cfg.CredentialHelpers)+1)
	helpers = append(helpers, cfg.CredentialsStore)

	for _, helper := range cfg.CredentialHelpers {
		helpers = append(helpers, helper)
	}

	return missingCredentialHelperBinaries(helpers)
}

func missingCredentialHelperBinaries(helpers []string) []string {
	helperByName := map[string]string{}

	for _, helper := range helpers {
		if helper == "" {
			continue
		}

		if _, exists := helperByName[helper]; exists {
			continue
		}

		binaryName := "docker-credential-" + helper
		if _, err := exec.LookPath(binaryName); err == nil {
			continue
		} else if errors.Is(err, exec.ErrNotFound) {
			helperByName[helper] = binaryName
		}
	}

	missing := make([]string, 0, len(helperByName))
	for helper, binaryName := range helperByName {
		missing = append(missing, fmt.Sprintf("%s (%s)", helper, binaryName))
	}

	sort.Strings(missing)

	return missing
}

// configuredHelper returns the credential helper configured for the given registry in the Docker config file.
// It checks for registry-specific helpers first, then falls back to the global credentials store if no specific helper is found.
func configuredHelper(cfg *configfile.ConfigFile, registry string) string {
	if cfg == nil {
		return ""
	}

	if helper, exists := cfg.CredentialHelpers[registry]; exists {
		return helper
	}

	switch registry {
	case dockerHubDomain, dockerHubIndexDomain, dockerHubRegistryHost:
		if helper, exists := cfg.CredentialHelpers[dockerHubAuthConfig]; exists {
			return helper
		}

		if helper, exists := cfg.CredentialHelpers[dockerHubDomain]; exists {
			return helper
		}

		if helper, exists := cfg.CredentialHelpers[dockerHubIndexDomain]; exists {
			return helper
		}
	}

	return cfg.CredentialsStore
}

// hasInlineAuth checks if the Docker config file contains inline authentication information
// for the given registry. It checks both the registry itself and its HTTPS variant.
func hasInlineAuth(cfg *configfile.ConfigFile, registry string) bool {
	if cfg == nil {
		return false
	}

	if _, ok := cfg.AuthConfigs[registry]; ok {
		return true
	}

	if _, ok := cfg.AuthConfigs["https://"+registry]; ok {
		return true
	}

	switch registry {
	case dockerHubDomain, dockerHubIndexDomain, dockerHubRegistryHost:
		if _, ok := cfg.AuthConfigs[dockerHubAuthConfig]; ok {
			return true
		}
	}

	return false
}
