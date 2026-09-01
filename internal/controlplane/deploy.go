package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/reconciliation"
	"github.com/kimdre/doco-cd/internal/source"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// SourcePreparer is the minimal source-preparation surface the Deployment
// operation depends on: resolving a deployment's source (Git repository or
// OCI artifact) into a ready-to-deploy local checkout. Satisfied by *source.Preparer.
type SourcePreparer interface {
	Prepare(ctx context.Context, req source.Request) (source.Result, error)
}

// Reconciler is the minimal reconciliation surface the Deployment operation
// depends on. Satisfied by *reconciliation.Manager.
type Reconciler interface {
	Deploy(ctx context.Context, req reconciliation.DeployRequest) error
}

// DeploymentDependencies configures the Deployment operation: the source
// preparer, the reconciler that runs the resolved deploy configs, the Docker
// CLI used to refresh swarm-mode capability, and the data mount point used to
// compute the source's internal/external filesystem paths.
type DeploymentDependencies struct {
	SourcePreparer SourcePreparer       `validate:"required"`
	Reconciler     Reconciler           `validate:"required"`
	DockerCLI      command.Cli          `validate:"required,nostructlevel"`
	DataMountPoint container.MountPoint `validate:"required"`
}

// Deployment is the protocol-neutral deployment operation shared by the
// webhook and poll transports: it refreshes the default swarm capability,
// prepares the source, adapts the result into a reconciliation deploy
// request, and runs it.
type Deployment struct {
	sourcePreparer SourcePreparer
	reconciler     Reconciler
	dockerCli      command.Cli
	dataMountPoint container.MountPoint
}

// NewDeployment validates dependencies and creates a Deployment operation.
func NewDeployment(dependencies DeploymentDependencies) (*Deployment, error) {
	if err := validation.Validate(dependencies); err != nil {
		return nil, fmt.Errorf("validate deployment dependencies: %w", err)
	}

	return &Deployment{
		sourcePreparer: dependencies.SourcePreparer,
		reconciler:     dependencies.Reconciler,
		dockerCli:      dependencies.DockerCLI,
		dataMountPoint: dependencies.DataMountPoint,
	}, nil
}

// DeploymentRequest bundles Deployment.Deploy's per-call, per-deployment-request
// input: the source location and its trigger/reference/visibility,
// notification metadata, an optional custom deploy target, an optional test
// identity, poll configuration (used only for poll-triggered requests), and
// the parsed webhook payload (zero value for non-webhook triggers).
type DeploymentRequest struct {
	Logger       *slog.Logger      `validate:"required,nostructlevel"`
	JobTrigger   stages.JobTrigger `validate:"required,oneof=webhook poll"`
	SourceType   config.SourceType
	SourceRef    string `validate:"required"`
	Ref          string
	Private      bool
	Metadata     notification.Metadata
	CustomTarget string
	TestName     string
	PollConfig   poll.Config
	Payload      webhook.ParsedPayload
}

// DeploymentError pairs a Deployment.Deploy failure with the HTTP status code
// transport layers (the webhook/poll handlers) should use to report it,
// preserving the exact message/status mapping the pre-refactor handler used.
type DeploymentError struct {
	Msg            string
	Err            error
	HTTPStatusCode int
}

func (e DeploymentError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Msg, e.Err)
	}

	return e.Msg
}

func (e DeploymentError) Unwrap() error {
	return e.Err
}

func newDeploymentError(msg string, err error, statusCode int) DeploymentError {
	return DeploymentError{Msg: msg, Err: err, HTTPStatusCode: statusCode}
}

// sourceFailureStatusCode classifies a source.Preparer.Prepare error into the
// HTTP status code the pre-refactor handler used for it.
func sourceFailureStatusCode(err error) int {
	switch {
	case errors.Is(err, source.ErrInvalidSourceType),
		errors.Is(err, source.ErrInvalidRepositoryName),
		errors.Is(err, source.ErrInvalidInternalPath),
		errors.Is(err, source.ErrInvalidExternalPath),
		errors.Is(err, source.ErrUnsupportedJobTrigger):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// sourceFailureMessage returns the exact error message the pre-refactor
// handler used for each classified source.Preparer.Prepare failure.
func sourceFailureMessage(err error) string {
	switch {
	case errors.Is(err, source.ErrInvalidRequest):
		return "invalid deployment request"
	case errors.Is(err, source.ErrInvalidSourceType):
		return "invalid source type"
	case errors.Is(err, source.ErrInvalidRepositoryName):
		return "invalid repository name"
	case errors.Is(err, source.ErrInvalidInternalPath):
		return "failed to verify and sanitize internal filesystem path"
	case errors.Is(err, source.ErrInvalidExternalPath):
		return "failed to verify and sanitize external filesystem path"
	case errors.Is(err, source.ErrGitClone):
		return "failed to clone repository"
	case errors.Is(err, source.ErrOCIResolveDigest):
		return "failed to resolve oci artifact digest"
	case errors.Is(err, source.ErrOCIVerify):
		return "failed OCI signature verification"
	case errors.Is(err, source.ErrOCIPull):
		return "failed to pull oci artifact"
	case errors.Is(err, source.ErrDeployConfig):
		return "failed to get deploy configuration"
	case errors.Is(err, source.ErrUnsupportedJobTrigger):
		return "unsupported job trigger"
	default:
		return "failed to prepare source"
	}
}

// Deploy runs a single protocol-neutral deployment: it validates req,
// refreshes the default swarm capability, prepares the source, adapts the
// result into a reconciliation.DeployRequest (notifying the deployment target
// observer for each resolved deploy config), and runs the reconciliation.
//
// Errors are returned as DeploymentError so callers can preserve the exact
// HTTP status/message mapping the pre-refactor handler used. A
// stages.ErrWebhookFilterMismatch from the reconciler is returned unwrapped so
// callers can keep reporting it as a skipped (not failed) deployment.
func (d *Deployment) Deploy(ctx context.Context, req DeploymentRequest) error {
	if err := validation.Validate(req); err != nil {
		return newDeploymentError("invalid deployment request", err, http.StatusInternalServerError)
	}

	if err := swarm.RefreshModeEnabled(ctx, d.dockerCli.Client()); err != nil {
		return newDeploymentError("failed to check if docker host is running in swarm mode", err, http.StatusInternalServerError)
	}

	result, err := d.sourcePreparer.Prepare(ctx, source.Request{
		Logger:         req.Logger,
		JobTrigger:     req.JobTrigger,
		SourceType:     req.SourceType,
		SourceRef:      req.SourceRef,
		Ref:            req.Ref,
		Private:        req.Private,
		CustomTarget:   req.CustomTarget,
		PollConfig:     req.PollConfig,
		Payload:        req.Payload,
		DataMountPoint: d.dataMountPoint,
	})
	if err != nil {
		return newDeploymentError(sourceFailureMessage(err), err, sourceFailureStatusCode(err))
	}

	for _, cfg := range result.DeployConfigs {
		if req.Metadata.DeploymentTargetObserver != nil {
			req.Metadata.DeploymentTargetObserver(cfg.Name, cfg.Context)
		}
	}

	repoData := stages.RepositoryData{
		Source:       result.SourceType,
		SourceUrl:    req.SourceRef,
		Name:         result.RepoName,
		PathInternal: result.PathInternal,
		PathExternal: result.PathExternal,
		Revision:     result.Revision,
		OCITrusted:   result.OCITrusted,
	}

	if err := d.reconciler.Deploy(ctx, reconciliation.DeployRequest{
		Logger:        req.Logger,
		Metadata:      req.Metadata,
		JobTrigger:    req.JobTrigger,
		Repository:    repoData,
		DeployConfigs: result.DeployConfigs,
		Payload:       &result.Payload,
		TestName:      req.TestName,
	}); err != nil {
		if errors.Is(err, stages.ErrWebhookFilterMismatch) {
			return err
		}

		return newDeploymentError("deployment failed", err, http.StatusInternalServerError)
	}

	return nil
}
