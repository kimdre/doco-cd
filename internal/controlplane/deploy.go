package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker"
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

// DockerContextResolver is the capability-aware Docker context surface used by Deployment.
type DockerContextResolver interface {
	Get(ctx context.Context, name string) (docker.ContextClient, error)
}

// DeploymentDependencies configures the source preparer, reconciler, and data
// mount point used by the protocol-neutral deployment operation.
type DeploymentDependencies struct {
	SourcePreparer SourcePreparer        `validate:"required"`
	Reconciler     Reconciler            `validate:"required"`
	Contexts       DockerContextResolver `validate:"required"`
	DataMountPoint container.MountPoint  `validate:"required"`
}

// Deployment is the protocol-neutral deployment operation shared by the
// webhook and poll transports.
type Deployment struct {
	sourcePreparer SourcePreparer
	reconciler     Reconciler
	contexts       DockerContextResolver
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
		contexts:       dependencies.Contexts,
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
	Response       error
	Cause          error
	HTTPStatusCode int
}

var (
	errSwarmModeCheck   = errors.New("failed to check if docker host is running in swarm mode")
	errDeploymentFailed = errors.New("deployment failed")
)

func (e DeploymentError) Error() string {
	switch {
	case e.Response == nil:
		if e.Cause == nil {
			return ""
		}

		return e.Cause.Error()
	case e.Cause != nil:
		return fmt.Sprintf("%s: %v", e.Response, e.Cause)
	default:
		return e.Response.Error()
	}
}

func (e DeploymentError) Unwrap() []error {
	switch {
	case e.Response == nil && e.Cause == nil:
		return nil
	case e.Response == nil:
		return []error{e.Cause}
	case e.Cause == nil:
		return []error{e.Response}
	default:
		return []error{e.Response, e.Cause}
	}
}

func newDeploymentError(response, cause error, statusCode int) DeploymentError {
	return DeploymentError{Response: response, Cause: cause, HTTPStatusCode: statusCode}
}

// classifySourceFailure returns the HTTP status code the pre-refactor handler
// used together with the source sentinel for a transport-safe response.
func classifySourceFailure(err error) (int, error) {
	switch {
	case errors.Is(err, source.ErrInvalidRequest):
		return http.StatusInternalServerError, source.ErrInvalidRequest
	case errors.Is(err, source.ErrInvalidSourceType):
		return http.StatusBadRequest, source.ErrInvalidSourceType
	case errors.Is(err, source.ErrInvalidRepositoryName):
		return http.StatusBadRequest, source.ErrInvalidRepositoryName
	case errors.Is(err, source.ErrInvalidInternalPath):
		return http.StatusBadRequest, source.ErrInvalidInternalPath
	case errors.Is(err, source.ErrInvalidExternalPath):
		return http.StatusBadRequest, source.ErrInvalidExternalPath
	case errors.Is(err, source.ErrUnsupportedJobTrigger):
		return http.StatusBadRequest, source.ErrUnsupportedJobTrigger
	case errors.Is(err, source.ErrGitClone):
		return http.StatusInternalServerError, source.ErrGitClone
	case errors.Is(err, source.ErrOCIResolveDigest):
		return http.StatusInternalServerError, source.ErrOCIResolveDigest
	case errors.Is(err, source.ErrOCIVerify):
		return http.StatusInternalServerError, source.ErrOCIVerify
	case errors.Is(err, source.ErrOCIPull):
		return http.StatusInternalServerError, source.ErrOCIPull
	case errors.Is(err, source.ErrDeployConfig):
		return http.StatusInternalServerError, source.ErrDeployConfig
	default:
		return http.StatusInternalServerError, source.ErrPrepare
	}
}

// Deploy runs a single protocol-neutral deployment: it validates req, prepares
// the source, adapts the result into a reconciliation.DeployRequest (notifying
// the deployment target observer for each resolved deploy config), and runs
// the reconciliation.
//
// Errors are returned as DeploymentError so callers can preserve the exact
// HTTP status/message mapping the pre-refactor handler used. A
// stages.ErrWebhookFilterMismatch from the reconciler is returned unwrapped so
// callers can keep reporting it as a skipped (not failed) deployment.
func (d *Deployment) Deploy(ctx context.Context, req DeploymentRequest) error {
	if err := validation.Validate(req); err != nil {
		return newDeploymentError(source.ErrInvalidRequest, err, http.StatusInternalServerError)
	}

	if _, err := d.contexts.Get(ctx, docker.DefaultContextName); err != nil {
		return newDeploymentError(errSwarmModeCheck, err, http.StatusInternalServerError)
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
		statusCode, response := classifySourceFailure(err)

		return newDeploymentError(response, err, statusCode)
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

		return newDeploymentError(errDeploymentFailed, err, http.StatusInternalServerError)
	}

	return nil
}
