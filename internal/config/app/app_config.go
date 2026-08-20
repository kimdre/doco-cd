package app

import (
	"errors"
	"fmt"
	"net/netip"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/notification"
)

const Name = "doco-cd" // Name of the application

var (
	Version            = "dev" // Version of the application, to be set during build time
	ErrInvalidLogLevel = validation.TextError{Err: errors.New("invalid log level, must be one of debug, info, warn, error")}
)

// Config is used to configure this application
// https://github.com/caarlos0/env?tab=readme-ov-file#env-tag-options
type Config struct {
	LogLevel                      string                 `env:"LOG_LEVEL,notEmpty" envDefault:"info"`                          // LogLevel is the log level for the application
	HttpPort                      uint16                 `env:"HTTP_PORT,notEmpty" envDefault:"80" validate:"min=1,max=65535"` // HttpPort is the port the HTTP server will listen on
	HttpTLSEnabled                bool                   `yaml:"-"`                                                            // HttpTLSEnabled indicates whether the main HTTP server serves HTTPS using the configured TLS certificate and key files.
	HttpTLSCertFile               string                 `env:"HTTP_TLS_CERT_FILE"`                                            // HttpTLSCertFile is the path to the TLS certificate PEM file used by the main HTTP server when configured.
	HttpTLSKeyFile                string                 `env:"HTTP_TLS_KEY_FILE"`                                             // HttpTLSKeyFile is the path to the TLS private key PEM file used by the main HTTP server when configured.
	HttpProxyString               string                 `env:"HTTP_PROXY"`                                                    // HttpProxyString is the HTTP proxy URL as a string
	HttpProxy                     transport.ProxyOptions // HttpProxy is the HTTP proxy configuration parsed from the HttpProxyString
	TrustedProxyNetworksString    string                 `env:"TRUSTED_PROXY_NETWORKS" envDefault:"127.0.0.0/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,::1/128"` // TrustedProxyNetworksString is the comma-separated list of trusted proxy CIDRs.
	TrustedProxyNetworks          []netip.Prefix         `yaml:"-"`                                                                                              // TrustedProxyNetworks holds parsed trusted proxy CIDR ranges.
	TrustedProxyHeader            string                 `env:"TRUSTED_PROXY_HEADER" envDefault:"X-Forwarded-For"`                                               // TrustedProxyHeader is an optional header name containing the original client IP.
	ApiSecret                     string                 `env:"API_SECRET"`                                                                                      // #nosec G117 -- ApiSecret is the secret token used to authenticate with the API
	ApiSecretFile                 string                 `env:"API_SECRET_FILE,file"`                                                                            // ApiSecretFile is the file containing the ApiSecret
	WebhookSecret                 string                 `env:"WEBHOOK_SECRET"`                                                                                  // WebhookSecret is the secret token used to authenticate the webhook
	WebhookSecretFile             string                 `env:"WEBHOOK_SECRET_FILE,file"`                                                                        // WebhookSecretFile is the file containing the WebhookSecret
	SourceURLRewritesYAML         string                 `env:"SOURCE_URL_REWRITES"`                                                                             // SourceURLRewritesYAML maps URL/domain/URI match rules to rewrite targets for git source URLs.
	SourceURLRewritesFile         string                 `env:"SOURCE_URL_REWRITES_FILE,file"`                                                                   // SourceURLRewritesFile is the file containing SourceURLRewritesYAML.
	SourceURLRewrites             map[string]string      `yaml:"-"`                                                                                              // SourceURLRewrites holds normalized source URL rewrite rules used by webhook and poll git sources.
	GitAccessToken                string                 `env:"GIT_ACCESS_TOKEN"`                                                                                // GitAccessToken is the access token used to authenticate with the Git server (e.g. GitHub) for private repositories
	GitAccessTokenUser            string                 `env:"GIT_ACCESS_TOKEN_USER,notEmpty" envDefault:"oauth2"`                                              // GitAccessTokenUser is the username paired with GitAccessToken for HTTP(S) clone/fetch. Needed by providers whose tokens have a fixed username (e.g. GitLab deploy tokens). Defaults to oauth2.
	GitCommitStatus               bool                   `env:"GIT_COMMIT_STATUS,notEmpty" envDefault:"false"`                                                   // GitCommitStatus controls whether doco-cd reports deployment outcomes as commit statuses to the source Git provider (requires GIT_ACCESS_TOKEN, or a configured GitHub App with "Commit statuses: Read and write" permission)
	GitScmProvider                string                 `env:"GIT_SCM_PROVIDER,notEmpty" envDefault:"auto"`                                                     // GitScmProvider overrides automatic SCM provider detection for commit statuses. Valid values: auto, github, gitlab, gitea, azuredevops (forgejo is accepted as an alias for gitea). Useful for self-hosted instances whose hostname does not reveal the product (e.g. git.mycompany.com running GitLab).
	GitScmApiUrl                  config.HttpUrl         `env:"GIT_SCM_API_URL" validate:"httpUrl"`                                                              // GitScmApiUrl overrides the inferred SCM API base URL for commit status calls (e.g. https://git.example.com).
	GitAccessTokenFile            string                 `env:"GIT_ACCESS_TOKEN_FILE,file"`                                                                      // GitAccessTokenFile is the file containing the GitAccessToken
	GitHubAppID                   string                 `env:"GITHUB_APP_ID"`                                                                                   // GitHubAppID is the GitHub App identifier used to mint installation access tokens
	GitHubAppIDFile               string                 `env:"GITHUB_APP_ID_FILE,file"`                                                                         // GitHubAppIDFile is the file containing the GitHub App identifier
	GitHubAppPrivateKey           string                 `env:"GITHUB_APP_PRIVATE_KEY"`                                                                          // GitHubAppPrivateKey is the PEM private key for the GitHub App
	GitHubAppPrivateKeyFile       string                 `env:"GITHUB_APP_PRIVATE_KEY_FILE,file"`                                                                // GitHubAppPrivateKeyFile is the file containing the GitHub App private key
	GitHubAppInstallationID       int64                  `env:"GITHUB_APP_INSTALLATION_ID"`                                                                      // GitHubAppInstallationID optionally pins a specific installation id (0 means auto-detect via owner/repo)
	GitAuthDomainsYAML            string                 `env:"GIT_AUTH_DOMAINS"`                                                                                // GitAuthDomainsYAML is the YAML configuration for domain-scoped Git credentials
	GitAuthDomainsFile            string                 `env:"GIT_AUTH_DOMAINS_FILE,file"`                                                                      // GitAuthDomainsFile is the file containing the YAML configuration for domain-scoped Git credentials
	GitAuthDomains                []git.ScopedAuthConfig `yaml:"-"`                                                                                              // GitAuthDomains holds parsed domain-scoped Git credentials
	GitCloneDepth                 int                    `env:"GIT_CLONE_DEPTH,notEmpty" envDefault:"0" validate:"min=0"`                                        // GitCloneDepth limits the number of commits to fetch. 0 means full clone (no depth limit). A positive value enables shallow clones.
	GitCloneSubmodules            bool                   `env:"GIT_CLONE_SUBMODULES,notEmpty" envDefault:"true"`                                                 // GitCloneSubmodules controls whether git submodules are cloned
	SSHPrivateKey                 string                 `env:"SSH_PRIVATE_KEY"`                                                                                 // SSHPrivateKey is the SSH private key used for SSH authentication with Git repositories
	SSHPrivateKeyFile             string                 `env:"SSH_PRIVATE_KEY_FILE,file"`                                                                       // SSHPrivateKeyFile is the file containing the SSHPrivateKey
	SSHPrivateKeyPassphrase       string                 `env:"SSH_PRIVATE_KEY_PASSPHRASE"`                                                                      // SSHPrivateKeyPassphrase is the passphrase for the SSH private key, if applicable
	SSHPrivateKeyPassphraseFile   string                 `env:"SSH_PRIVATE_KEY_PASSPHRASE_FILE,file"`                                                            // SSHPrivateKeyPassphraseFile is the file containing the SSHPrivateKeyPassphrase
	SkipTLSVerification           bool                   `env:"SKIP_TLS_VERIFICATION,notEmpty" envDefault:"false"`                                               // SkipTLSVerification skips the TLS verification when cloning repositories.
	DockerQuietDeploy             bool                   `env:"DOCKER_QUIET_DEPLOY,notEmpty" envDefault:"true"`                                                  // DockerQuietDeploy suppresses the status output of dockerCli in deployments (e.g. pull, create, start)
	SchedulerEnabled              bool                   `env:"SCHEDULER_ENABLED,notEmpty" envDefault:"true"`                                                    // SchedulerEnabled controls whether the built-in scheduled job runner is started in this doco-cd instance
	McpEnabled                    bool                   `env:"MCP_ENABLED,notEmpty" envDefault:"false"`                                                         // McpEnabled enables the built-in MCP server and requires API_SECRET.
	DockerSwarmFeatures           bool                   `env:"DOCKER_SWARM_FEATURES,notEmpty" envDefault:"true"`                                                // DockerSwarmFeatures enables the usage Docker Swarm features in the application if it has detected that it is running in a Docker Swarm environment
	DockerSwarmConfigRetention    int                    `env:"DOCKER_SWARM_CONFIG_RETENTION,notEmpty" envDefault:"0" validate:"min=-1"`                         // DockerSwarmConfigRetention is the global default number of old Swarm config revisions to keep per resource (excluding the active one). -1 disables automatic pruning.
	DockerSwarmSecretRetention    int                    `env:"DOCKER_SWARM_SECRET_RETENTION,notEmpty" envDefault:"0" validate:"min=-1"`                         // DockerSwarmSecretRetention is the global default number of old Swarm secret revisions to keep per resource (excluding the active one). -1 disables automatic pruning.
	DataMountPath                 string                 `env:"DATA_MOUNT_PATH" envDefault:"/data"`                                                              // DataMountPath is the expected mount path inside the container for the writable deployment data volume.
	DataHostPath                  string                 `env:"DATA_HOST_PATH"`                                                                                  // DataHostPath is the optional Docker daemon host path backing DataMountPath.
	DeployConfigBaseDir           string                 `env:"DEPLOY_CONFIG_BASE_DIR" envDefault:"/"`                                                           // DeployConfigBaseDir is the base directory (relative to the repository root) where deployment configuration files will be searched for.
	PassEnv                       bool                   `env:"PASS_ENV"`                                                                                        // PassEnv controls whether environment variables from the doco-cd container should be passed to the deployment environment for docker compose variable interpolation. Use with caution, as this may expose sensitive information to the deployment environment.
	PollConfigYAML                string                 `env:"POLL_CONFIG"`                                                                                     // PollConfigYAML is the unparsed string containing the PollConfig in YAML format
	PollConfigFile                string                 `env:"POLL_CONFIG_FILE,file"`                                                                           // PollConfigFile is the file containing the PollConfig in YAML format
	PollConfig                    []poll.Config          `yaml:"-"`                                                                                              // PollConfig is the YAML configuration for polling Git repositories for changes
	MaxPayloadSize                int64                  `env:"MAX_PAYLOAD_SIZE,notEmpty" envDefault:"1048576"`                                                  // MaxPayloadSize is the maximum size of the payload in bytes that the HTTP server will accept (default 1MB = 1048576 bytes)
	MetricsPort                   uint16                 `env:"METRICS_PORT,notEmpty" envDefault:"9120" validate:"min=1,max=65535"`                              // MetricsPort is the port the prometheus metrics server will listen on
	AppriseApiURL                 config.HttpUrl         `env:"APPRISE_API_URL" validate:"httpUrl"`                                                              // AppriseApiURL is the URL of the Apprise notification service
	AppriseNotifyUrls             string                 `env:"APPRISE_NOTIFY_URLS"`                                                                             // AppriseNotifyUrls is a comma-separated list of URLs to notify via the Apprise notification service
	AppriseNotifyUrlsFile         string                 `env:"APPRISE_NOTIFY_URLS_FILE,file"`                                                                   // AppriseNotifyUrlsFile is the file containing the AppriseNotifyUrls
	AppriseNotifyLevel            string                 `env:"APPRISE_NOTIFY_LEVEL,notEmpty" envDefault:"success"`                                              // AppriseNotifyLevel is the level of notifications to send via the Apprise notification service
	AppriseNotifyRepeatInterval   time.Duration          `env:"APPRISE_NOTIFY_REPEAT_INTERVAL,notEmpty" envDefault:"1h"`                                         // AppriseNotifyRepeatInterval is how long an unchanged failure notification is suppressed before it is sent again as a reminder. Zero or less sends every failure.
	AppriseNotifyBodyTemplate     string                 `env:"APPRISE_NOTIFY_BODY_TEMPLATE"`                                                                    // AppriseNotifyBodyTemplate is an optional Go text/template rendering the notification body. Empty uses the built-in format.
	AppriseNotifyBodyTemplateFile string                 `env:"APPRISE_NOTIFY_BODY_TEMPLATE_FILE,file"`                                                          // AppriseNotifyBodyTemplateFile is the file containing the AppriseNotifyBodyTemplate
	SecretProvider                string                 `env:"SECRET_PROVIDER"`                                                                                 // SecretProvider is the secret provider/manager to use for retrieving secrets (e.g. bitwarden secrets manager)
	MaxDeploymentLoopCount        uint                   `env:"MAX_DEPLOYMENT_LOOP_COUNT,notEmpty" envDefault:"2" validate:"min=0"`                              // Maximum allowed deployment loops before a forced deployment is triggered
	MaxConcurrentDeployments      uint                   `env:"MAX_CONCURRENT_DEPLOYMENTS,notEmpty" envDefault:"4" validate:"min=1"`                             // Maximum number of concurrent deployments allowed
	OciVerifyMaxWorkers           uint                   `env:"OCI_VERIFY_MAX_WORKERS,notEmpty" envDefault:"1" validate:"min=1,max=10"`                          // Maximum number of workers used per OCI signature verification (clamped to 10)
	OciTrustPolicyYAML            string                 `env:"OCI_TRUST_POLICY"`                                                                                // OciTrustPolicyYAML contains the app-level OCI signature trust policy as YAML
	OciTrustPolicyFile            string                 `env:"OCI_TRUST_POLICY_FILE,file"`                                                                      // OciTrustPolicyFile is the file containing OCI trust policy YAML
	OciTrustPolicy                config.OciTrustPolicy  `yaml:"-"`                                                                                              // OciTrustPolicy is the parsed app-level OCI signature trust policy
	OciInsecureRegistriesString   string                 `env:"OCI_INSECURE_REGISTRIES"`                                                                         // OciInsecureRegistriesString is a comma-separated list of registries for OCI Compose includes with TLS verification disabled.
	OciInsecureRegistries         []string               `yaml:"-"`                                                                                              // OciInsecureRegistries holds normalized OCI Compose include registries with TLS verification disabled.
	CertRotationEnabled           bool                   `env:"CERT_ROTATION_ENABLED,notEmpty" envDefault:"false"`                                               // CertRotationEnabled enables the built-in watcher that automatically rotates certificates issued through rotation-capable external secret providers (e.g. OpenBao PKI roles)
	CertRotationThreshold         time.Duration          `env:"CERT_ROTATION_THRESHOLD,notEmpty" envDefault:"72h" validate:"min=1"`                              // CertRotationThreshold is how far ahead of a certificate's expiry doco-cd triggers a rotation
	CertRotationCheckInterval     time.Duration          `env:"CERT_ROTATION_CHECK_INTERVAL,notEmpty" envDefault:"1h" validate:"min=1"`                          // CertRotationCheckInterval is how often the certificate rotation watcher checks deployed certificates for upcoming expiry
}

// GetConfig returns the app Config.
func GetConfig() (*Config, error) {
	cfg := Config{}

	mappings := []config.EnvVarFileMapping{
		{EnvName: "API_SECRET", EnvValue: &cfg.ApiSecret, FileValue: &cfg.ApiSecretFile, AllowUnset: true},
		{EnvName: "APPRISE_NOTIFY_URLS", EnvValue: &cfg.AppriseNotifyUrls, FileValue: &cfg.AppriseNotifyUrlsFile, AllowUnset: true},
		{EnvName: "APPRISE_NOTIFY_BODY_TEMPLATE", EnvValue: &cfg.AppriseNotifyBodyTemplate, FileValue: &cfg.AppriseNotifyBodyTemplateFile, AllowUnset: true},
		{EnvName: "GIT_ACCESS_TOKEN", EnvValue: &cfg.GitAccessToken, FileValue: &cfg.GitAccessTokenFile, AllowUnset: true},
		{EnvName: "GITHUB_APP_ID", EnvValue: &cfg.GitHubAppID, FileValue: &cfg.GitHubAppIDFile, AllowUnset: true},
		{EnvName: "GITHUB_APP_PRIVATE_KEY", EnvValue: &cfg.GitHubAppPrivateKey, FileValue: &cfg.GitHubAppPrivateKeyFile, AllowUnset: true},
		{EnvName: "GIT_AUTH_DOMAINS", EnvValue: &cfg.GitAuthDomainsYAML, FileValue: &cfg.GitAuthDomainsFile, AllowUnset: true},
		{EnvName: "OCI_TRUST_POLICY", EnvValue: &cfg.OciTrustPolicyYAML, FileValue: &cfg.OciTrustPolicyFile, AllowUnset: true},
		{EnvName: "SSH_PRIVATE_KEY", EnvValue: &cfg.SSHPrivateKey, FileValue: &cfg.SSHPrivateKeyFile, AllowUnset: true},
		{EnvName: "SSH_PRIVATE_KEY_PASSPHRASE", EnvValue: &cfg.SSHPrivateKeyPassphrase, FileValue: &cfg.SSHPrivateKeyPassphraseFile, AllowUnset: true},
		{EnvName: "WEBHOOK_SECRET", EnvValue: &cfg.WebhookSecret, FileValue: &cfg.WebhookSecretFile, AllowUnset: true},
		{EnvName: "SOURCE_URL_REWRITES", EnvValue: &cfg.SourceURLRewritesYAML, FileValue: &cfg.SourceURLRewritesFile, AllowUnset: true},
	}

	err := config.ParseConfigFromEnv(&cfg, &mappings)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	if cfg.McpEnabled && cfg.ApiSecret == "" {
		return nil, errors.New("MCP_ENABLED requires API_SECRET")
	}

	err = cfg.parsePollConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to parse poll config: %w", err)
	}

	err = cfg.parseGitAuthDomains()
	if err != nil {
		return nil, fmt.Errorf("failed to parse GIT_AUTH_DOMAINS: %w", err)
	}

	err = cfg.parseSourceURLRewrites()
	if err != nil {
		return nil, fmt.Errorf("failed to parse SOURCE_URL_REWRITES: %w", err)
	}

	err = cfg.validateGitAuthConfig()
	if err != nil {
		return nil, err
	}

	err = cfg.parseOciTrustPolicy()
	if err != nil {
		return nil, fmt.Errorf("failed to parse OCI_TRUST_POLICY: %w", err)
	}

	cfg.TrustedProxyNetworks, err = config.ParseTrustedProxyNetworks(cfg.TrustedProxyNetworksString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse TRUSTED_PROXY_NETWORKS: %w", err)
	}

	cfg.OciInsecureRegistries, err = config.ParseOciInsecureRegistries(cfg.OciInsecureRegistriesString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse OCI_INSECURE_REGISTRIES: %w", err)
	}

	if _, err = commitstatus.ParseProvider(cfg.GitScmProvider); err != nil {
		return nil, fmt.Errorf("invalid GIT_SCM_PROVIDER: %w", err)
	}

	for _, pollConfig := range cfg.PollConfig {
		if err = pollConfig.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %w", poll.ErrInvalidConfig, err)
		}
	}

	logLvl := strings.ToLower(cfg.LogLevel)
	if logLvl != "debug" && logLvl != "info" && logLvl != "warn" && logLvl != "error" {
		return nil, ErrInvalidLogLevel
	}

	if err = validation.Validate(cfg); err != nil {
		return nil, err
	}

	if cfg.HttpPort == cfg.MetricsPort {
		return nil, fmt.Errorf("HTTP_PORT and METRICS_PORT cannot be the same port number: %d", cfg.HttpPort)
	}

	cfg.HttpTLSCertFile = strings.TrimSpace(cfg.HttpTLSCertFile)
	cfg.HttpTLSKeyFile = strings.TrimSpace(cfg.HttpTLSKeyFile)

	if (cfg.HttpTLSCertFile == "") != (cfg.HttpTLSKeyFile == "") {
		return nil, errors.New("HTTP_TLS_CERT_FILE and HTTP_TLS_KEY_FILE must be set together")
	}

	cfg.HttpTLSEnabled = cfg.HttpTLSCertFile != "" && cfg.HttpTLSKeyFile != ""

	if cfg.HttpProxyString != "" {
		cfg.HttpProxy = transport.ProxyOptions{
			URL: cfg.HttpProxyString,
		}

		err = cfg.HttpProxy.Validate()
		if err != nil {
			return nil, fmt.Errorf("failed to validate HTTP_PROXY: %w", err)
		}
	}

	dataMountPath := strings.TrimSpace(cfg.DataMountPath)
	if dataMountPath == "" {
		dataMountPath = "/data"
	}

	cfg.DataMountPath = path.Clean(dataMountPath)
	if !path.IsAbs(cfg.DataMountPath) {
		return nil, fmt.Errorf("DATA_MOUNT_PATH must be an absolute Unix path: %q", cfg.DataMountPath)
	}

	cfg.DataHostPath = strings.TrimSpace(cfg.DataHostPath)
	if cfg.DataHostPath != "" {
		cfg.DataHostPath = path.Clean(cfg.DataHostPath)
		if !path.IsAbs(cfg.DataHostPath) {
			return nil, fmt.Errorf("DATA_HOST_PATH must be an absolute Unix path: %q", cfg.DataHostPath)
		}
	}

	notification.SetFailureRepeatInterval(cfg.AppriseNotifyRepeatInterval)

	err = notification.SetAppriseConfig(
		string(cfg.AppriseApiURL),
		cfg.AppriseNotifyUrls,
		cfg.AppriseNotifyLevel,
		cfg.AppriseNotifyBodyTemplate,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to configure notifications: %w", err)
	}

	return &cfg, nil
}

func (cfg *Config) parseOciTrustPolicy() error {
	if strings.TrimSpace(cfg.OciTrustPolicyYAML) == "" {
		cfg.OciTrustPolicy = config.OciTrustPolicy{
			Enabled: false,
		}

		return nil
	}

	if err := yaml.Unmarshal([]byte(cfg.OciTrustPolicyYAML), &cfg.OciTrustPolicy); err != nil {
		return err
	}

	cfg.OciTrustPolicy = config.NormalizeOciTrustPolicy(cfg.OciTrustPolicy)

	return nil
}

// parseGitAuthDomains parses domain-scoped Git credentials from GIT_AUTH_DOMAINS (or *_FILE content).
func (cfg *Config) parseGitAuthDomains() error {
	if strings.TrimSpace(cfg.GitAuthDomainsYAML) == "" {
		cfg.GitAuthDomains = []git.ScopedAuthConfig{}

		return nil
	}

	if err := yaml.Unmarshal([]byte(cfg.GitAuthDomainsYAML), &cfg.GitAuthDomains); err != nil {
		return err
	}

	return nil
}

func (cfg *Config) parseSourceURLRewrites() error {
	cfg.SourceURLRewrites = map[string]string{}

	if strings.TrimSpace(cfg.SourceURLRewritesYAML) == "" {
		return nil
	}

	raw := map[string]string{}
	if err := yaml.Unmarshal([]byte(cfg.SourceURLRewritesYAML), &raw); err != nil {
		return err
	}

	for key, value := range raw {
		normalizedKey := strings.ToLower(strings.TrimSpace(key))
		normalizedURL := strings.TrimSpace(value)

		if normalizedKey == "" {
			return errors.New("rewrite key in SOURCE_URL_REWRITES must not be empty")
		}

		if normalizedURL == "" {
			return fmt.Errorf("rewrite key %q in SOURCE_URL_REWRITES must have a non-empty target", key)
		}

		cfg.SourceURLRewrites[normalizedKey] = normalizedURL
	}

	return nil
}

func (cfg *Config) validateGitAuthConfig() error {
	cfg.GitHubAppID = strings.TrimSpace(cfg.GitHubAppID)
	cfg.GitHubAppPrivateKey = strings.TrimSpace(cfg.GitHubAppPrivateKey)

	globalToken := strings.TrimSpace(cfg.GitAccessToken)

	hasCompleteGlobalApp := cfg.GitHubAppID != "" && cfg.GitHubAppPrivateKey != ""
	if hasCompleteGlobalApp {
		if globalToken != "" {
			return errors.New("GIT_ACCESS_TOKEN cannot be combined with global GitHub App credentials")
		}
	} else {
		// Incomplete global app credentials are ignored to keep startup resilient in mixed environments.
		cfg.GitHubAppID = ""
		cfg.GitHubAppPrivateKey = ""
		cfg.GitHubAppInstallationID = 0
	}

	for i, entry := range cfg.GitAuthDomains {
		hasToken := strings.TrimSpace(entry.GitAccessToken) != ""
		hasSSH := strings.TrimSpace(entry.SSHPrivateKey) != ""
		hasApp := strings.TrimSpace(entry.GitHubAppID) != "" || strings.TrimSpace(entry.GitHubAppPrivateKey) != ""

		if hasApp {
			if strings.TrimSpace(entry.GitHubAppID) == "" || strings.TrimSpace(entry.GitHubAppPrivateKey) == "" {
				return fmt.Errorf("GIT_AUTH_DOMAINS[%d]: both github_app_id and github_app_private_key are required", i)
			}

			if hasToken || hasSSH {
				return fmt.Errorf("GIT_AUTH_DOMAINS[%d]: github app credentials cannot be combined with git_access_token or ssh_private_key", i)
			}
		}
	}

	return nil
}

// parsePollConfig parses the PollConfig from either the PollConfigYAML string or the PollConfigFile.
func (cfg *Config) parsePollConfig() error {
	if cfg.PollConfigYAML != "" && cfg.PollConfigFile != "" {
		return poll.ErrBothConfigSet
	}

	if cfg.PollConfigYAML != "" {
		return yaml.Unmarshal([]byte(cfg.PollConfigYAML), &cfg.PollConfig)
	}

	if cfg.PollConfigFile != "" {
		return yaml.Unmarshal([]byte(cfg.PollConfigFile), &cfg.PollConfig)
	}

	cfg.PollConfig = []poll.Config{} // Default to an empty slice if no config is provided

	return nil
}
