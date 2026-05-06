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
}
