package deploy

import "github.com/go-git/go-git/v5/plumbing/transport"

const DefaultReference = "refs/heads/main"

// GitOptions holds git-related options for operations that require cloning or fetching remote repositories.
type GitOptions struct {
	SSHPrivateKey           string
	SSHPrivateKeyPassphrase string
	GitAccessToken          string
	SkipTLSVerification     bool
	HttpProxy               transport.ProxyOptions
	GitCloneSubmodules      bool
	GitCloneDepth           int
	SourceURL               string
	// PrimaryReference overrides the reference known to resolve to primaryRevision.
	// Empty uses the job's reference. Reloads can use the artifact's commit hash
	// when the config-source branch is unknown or differs from the deployment's.
	PrimaryReference string
	// SourceBaseDir is the directory every source's store lives under (DATA_MOUNT_PATH).
	// Auto-discovery of a config that names its own repository_url needs it to place that repository's store beside the
	// others, since repoRoot is a published artifact directory and nothing about that path leads back to the shared root.
	SourceBaseDir string
}
