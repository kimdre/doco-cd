// Package source resolves a deployment's source, either a Git repository or an OCI
// artifact, into a ready-to-deploy local checkout.
//
// It owns:
//   - source type normalization/validation
//   - repository/artifact naming and safe internal/external filesystem paths
//   - Git clone/update, including the HeadMatchesCommit fast path that skips
//     the network fetch when the local checkout already matches the requested
//     commit, and resolving the immutable revision (commit SHA)
//   - OCI digest resolution, cosign signature verification, pull/extract, and
//     webhook payload enrichment
//   - webhook/poll deployment configuration resolution, OCI reference
//     override, and custom target propagation
//
// It intentionally does not depend on internal/controlplane or
// internal/reconciliation: callers adapt Result into their own deployment
// request types.
package source

import (
	"errors"
	"fmt"

	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
)

// Sentinel errors classify Prepare failures so callers can map them to the
// right transport response (e.g. an HTTP status code) without string
// matching. Errors returned by Prepare wrap these sentinels via
// wrapPrepareError so errors.Is finds both the sentinel and the underlying
// detail error, while Error() returns only the detail error's message -
// matching the pre-refactor handler's HTTP response bodies exactly.
var (
	// ErrPrepare indicates an unclassified source preparation failure.
	ErrPrepare = errors.New("failed to prepare source")
	// ErrInvalidRequest indicates the Request itself failed validation.
	ErrInvalidRequest = errors.New("invalid deployment request")
	// ErrInvalidSourceType indicates an unsupported/invalid SourceType.
	ErrInvalidSourceType = errors.New("invalid source type")
	// ErrInvalidRepositoryName indicates a repository/artifact name that is unsafe to use as a path component.
	ErrInvalidRepositoryName = errors.New("invalid repository name")
	// ErrInvalidInternalPath indicates the computed internal (in-container) filesystem path escaped the data mount point.
	ErrInvalidInternalPath = errors.New("failed to verify and sanitize internal filesystem path")
	// ErrInvalidExternalPath indicates the computed external (host) filesystem path escaped the data mount point.
	ErrInvalidExternalPath = errors.New("failed to verify and sanitize external filesystem path")
	// ErrGitClone indicates a Git clone/update failure.
	ErrGitClone = errors.New("failed to clone repository")
	// ErrOCIResolveDigest indicates an OCI digest resolution failure.
	ErrOCIResolveDigest = errors.New("failed to resolve oci artifact digest")
	// ErrOCIVerify indicates an OCI cosign signature verification failure.
	ErrOCIVerify = errors.New("failed OCI signature verification")
	// ErrOCIPull indicates an OCI artifact pull/extract failure.
	ErrOCIPull = errors.New("failed to pull oci artifact")
	// ErrDeployConfig indicates a webhook/poll deployment configuration resolution failure.
	ErrDeployConfig = errors.New("failed to get deploy configuration")
	// ErrUnsupportedJobTrigger indicates an unrecognized JobTrigger.
	ErrUnsupportedJobTrigger = errors.New("unsupported job trigger")
)

// prepareError pairs a classification sentinel with the underlying detail
// error. Error() returns only the detail error's message (so callers that
// surface it, e.g. as an HTTP response body, see the same text the
// pre-refactor handler returned), while Unwrap exposes both the sentinel and
// the detail error to errors.Is/errors.As.
type prepareError struct {
	sentinel error
	detail   error
}

// wrapPrepareError returns an error classified as sentinel (matched by
// errors.Is(result, sentinel)) whose message is exactly detail's message.
func wrapPrepareError(sentinel, detail error) error {
	return &prepareError{sentinel: sentinel, detail: detail}
}

func (e *prepareError) Error() string {
	return e.detail.Error()
}

func (e *prepareError) Unwrap() []error {
	return []error{e.sentinel, e.detail}
}

// Dependencies holds the stable services shared by every Preparer.Prepare call.
type Dependencies struct {
	AppConfig *app.Config `validate:"required,nostructlevel"`
}

// Preparer resolves sources (Git repositories or OCI artifacts) into a
// ready-to-deploy local checkout. See the package doc for its responsibilities.
type Preparer struct {
	appConfig *app.Config
}

// NewPreparer validates dependencies and creates a Preparer.
func NewPreparer(dependencies Dependencies) (*Preparer, error) {
	if err := validation.Validate(dependencies); err != nil {
		return nil, fmt.Errorf("validate source dependencies: %w", err)
	}

	return &Preparer{appConfig: dependencies.AppConfig}, nil
}

// EntityLabel returns the log-friendly entity name for sourceType: "artifact"
// for OCI sources, "repository" otherwise.
func EntityLabel(sourceType config.SourceType) string {
	if config.NormalizeSourceType(sourceType) == config.SourceTypeOCI {
		return "artifact"
	}

	return "repository"
}
