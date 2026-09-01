package source

import (
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// Result is the outcome of a successful Preparer.Prepare call: the resolved
// source identity/paths/revision, the resolved deploy configurations (with
// custom target propagation and, for OCI, reference override already
// applied), and the payload (enriched for OCI sources; passed through
// unchanged for Git sources).
type Result struct {
	SourceType    config.SourceType // Source backend used for this deployment (git or oci)
	RepoName      string            // Repository/artifact name (e.g., "user/my-repo")
	PathInternal  string            // Path to the repository/artifact inside the container
	PathExternal  string            // Path to the repository/artifact on the host machine
	Revision      string            // Resolved immutable revision (commit SHA or OCI digest)
	OCITrusted    bool              // True when the OCI artifact passed trust-policy verification (always true for Git)
	DeployConfigs []*deploy.Config  // Resolved deployment configurations for this run
	Payload       webhook.ParsedPayload
}
