package docker

import (
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/config/app"
)

// ComposeLoadOptions bundles the Docker-owned settings LoadCompose needs beyond the individual
// compose project parameters (compose/env files, profiles, environment). These settings originate
// in the application configuration, but callers resolve and pass them in explicitly at a
// composition/stage boundary (see NewComposeLoadOptions) instead of letting LoadCompose read the
// application configuration itself. This keeps LoadCompose free of hidden environment reads.
type ComposeLoadOptions struct {
	// PassEnv controls whether the doco-cd process's own OS environment variables are passed
	// through to the compose project for variable interpolation.
	PassEnv bool
	// SkipTLSVerify skips TLS verification when following Git remote includes.
	SkipTLSVerify bool
	// HttpProxy configures the proxy used when following Git remote includes.
	HttpProxy transport.ProxyOptions
	// GitCloneSubmodules controls whether submodules are cloned for Git remote includes.
	GitCloneSubmodules bool
	// GitCloneDepth limits the number of commits fetched for Git remote includes. 0 means a full clone.
	GitCloneDepth int
	// SSHPrivateKey and SSHPrivateKeyPassphrase configure SSH authentication for Git remote includes.
	SSHPrivateKey           string
	SSHPrivateKeyPassphrase string
	// GitAccessToken configures HTTP(S) authentication for Git remote includes.
	GitAccessToken string
	// OciInsecureRegistries lists registries for OCI Compose includes with TLS verification disabled.
	OciInsecureRegistries []string
	// DataHostPath and DataMountPath are used as a fallback base directory for the Git include
	// cache when the repository path passed to LoadCompose is empty. DataHostPath (the Docker
	// daemon host path) is preferred since the cache must be reachable outside the container by
	// the daemon performing the checkout.
	DataHostPath  string
	DataMountPath string
}

// NewComposeLoadOptions builds the ComposeLoadOptions LoadCompose needs from the application
// configuration. Call this once at a composition/stage boundary and pass the result down
// explicitly, rather than letting deep Docker helpers read the application configuration
// themselves.
func NewComposeLoadOptions(c *app.Config) ComposeLoadOptions {
	if c == nil {
		return ComposeLoadOptions{}
	}

	return ComposeLoadOptions{
		PassEnv:                 c.PassEnv,
		SkipTLSVerify:           c.SkipTLSVerification,
		HttpProxy:               c.HttpProxy,
		GitCloneSubmodules:      c.GitCloneSubmodules,
		GitCloneDepth:           c.GitCloneDepth,
		SSHPrivateKey:           c.SSHPrivateKey,
		SSHPrivateKeyPassphrase: c.SSHPrivateKeyPassphrase,
		GitAccessToken:          c.GitAccessToken,
		OciInsecureRegistries:   c.OciInsecureRegistries,
		DataHostPath:            c.DataHostPath,
		DataMountPath:           c.DataMountPath,
	}
}

// ScheduledComposeOptions bundles the Docker-owned settings needed to reload the Compose project
// for a scheduled job (see RunComposeScheduledContainer/RunComposeOneOffFromServiceDefinition) or
// for certificate rotation (see CertificateRotationOptions), beyond what LoadCompose itself needs.
type ScheduledComposeOptions struct {
	// ComposeLoad is passed through to the LoadCompose call used to reload the project.
	ComposeLoad ComposeLoadOptions
	// DeployConfigBaseDir is the base directory (relative to the repository root) where
	// deployment configuration files are searched for.
	DeployConfigBaseDir string
	// InterpolateExternalSecrets enables Compose-style interpolation in legacy external secret
	// references using the doco-cd process environment.
	InterpolateExternalSecrets bool
}

// NewScheduledComposeOptions builds ScheduledComposeOptions from the application configuration.
// Call this once at a composition/stage boundary and pass the result down explicitly.
func NewScheduledComposeOptions(c *app.Config) ScheduledComposeOptions {
	if c == nil {
		return ScheduledComposeOptions{}
	}

	return ScheduledComposeOptions{
		ComposeLoad:                NewComposeLoadOptions(c),
		DeployConfigBaseDir:        c.DeployConfigBaseDir,
		InterpolateExternalSecrets: c.InterpolateExternalSecrets,
	}
}

// CertificateRotationOptions bundles the Docker-owned settings needed by RotateProjectCertificates
// beyond the deployment labels it is given: the settings needed to reload the scheduled Compose
// project (see ScheduledComposeOptions) plus the global Swarm config/secret revision retention
// defaults used to prune superseded revisions after a Swarm rotation redeploy.
type CertificateRotationOptions struct {
	// Scheduled is passed through to reload the deploy config/compose project being rotated.
	Scheduled ScheduledComposeOptions
	// DockerSwarmConfigRetention is the global default number of old Swarm config revisions to
	// keep per resource (excluding the active one). -1 disables automatic pruning.
	DockerSwarmConfigRetention int
	// DockerSwarmSecretRetention is the global default number of old Swarm secret revisions to
	// keep per resource (excluding the active one). -1 disables automatic pruning.
	DockerSwarmSecretRetention int
}

// NewCertificateRotationOptions builds CertificateRotationOptions from the application
// configuration. Call this once at a composition/stage boundary and pass the result down
// explicitly.
func NewCertificateRotationOptions(c *app.Config) CertificateRotationOptions {
	if c == nil {
		return CertificateRotationOptions{}
	}

	return CertificateRotationOptions{
		Scheduled:                  NewScheduledComposeOptions(c),
		DockerSwarmConfigRetention: c.DockerSwarmConfigRetention,
		DockerSwarmSecretRetention: c.DockerSwarmSecretRetention,
	}
}
