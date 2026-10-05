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
	// Empty uses the job's reference. A config-source artifact's commit hash
	// identifies its content when the branch that supplied it is unknown.
	PrimaryReference string
	// DiscoverySnapshot restores only deployments discovered in the recorded source and reference.
	// When set, discovery reads this snapshot without resolving or fetching any references.
	DiscoverySnapshot *DiscoverySnapshot
	// SourceBaseDir is the directory every source's store lives under (DATA_MOUNT_PATH).
	// Auto-discovery of a config that names its own repository_url needs it to place that repository's store beside the
	// others, since repoRoot is a published artifact directory and nothing about that path leads back to the shared root.
	SourceBaseDir string
}

// DiscoverySnapshot identifies the materialized source of an existing deployment.
// RepositoryRoot can differ from the artifact containing its top-level deploy config.
// MirrorDir and a Git commit Revision enable Git-backed discovery; other snapshots are scanned from disk.
type DiscoverySnapshot struct {
	RepositoryRoot string
	MirrorDir      string
	Revision       string
	Reference      string
}
