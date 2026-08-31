package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/moby/moby/api/types/container"

	types2 "github.com/kimdre/doco-cd/internal/config"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/common/types/slice"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/notification"

	gitInternal "github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/webhook"
)

var (
	ErrNotManagedByDocoCD = errors.New("stack is not managed by doco-cd")
	ErrDeploymentConflict = errors.New("another stack with the same name already exists and is not managed by this repository")
	ErrSkipDeployment     = errors.New("deployment skipped") // Special error to indicate deployment was skipped, not an actual failure/error

	// ErrWebhookFilterMismatch is returned when the deployment is skipped because
	// the webhook ref does not match the configured webhook_filter. It wraps
	// ErrSkipDeployment so existing errors.Is checks still match, but callers can
	// distinguish it to return an appropriate "skipped" response instead of "success".
	ErrWebhookFilterMismatch = fmt.Errorf("webhook filter did not match: %w", ErrSkipDeployment)
)

type StageName string

type StageResult string

type StageStatus string

const (
	StageInit        StageName = "init"
	StagePreDeploy   StageName = "pre-deploy"
	StageDestroy     StageName = "destroy"
	StageDeploy      StageName = "deploy"
	StagePostDeploy  StageName = "post-deploy"
	StagePostDestroy StageName = "post-destroy"
	StageCleanup     StageName = "cleanup"
)

type JobTrigger string

const (
	JobTriggerWebhook JobTrigger = "webhook"
	JobTriggerPoll    JobTrigger = "poll"
)

type MetaData struct {
	Name       StageName
	StartedAt  time.Time
	FinishedAt time.Time
}

// InitStageData holds the configuration and data specific to the initialization stage.
type InitStageData struct {
	MetaData
}

// PreDeployStageData holds the configuration and data specific to the pre-deployment stage.
type PreDeployStageData struct {
	MetaData
}

// DeployStageData holds the configuration and data specific to the deployment stage.
type DeployStageData struct {
	MetaData
}

type DestroyStageData struct {
	MetaData
}

// PostDeployStageData holds the configuration and data specific to the post-deployment stage.
type PostDeployStageData struct {
	MetaData
}

type PostDestroyStageData struct {
	MetaData
}

// CleanupStageData holds the configuration and data specific to the cleanup stage.
type CleanupStageData struct {
	MetaData
}

func NewMetaData(name StageName) MetaData {
	return MetaData{
		Name: name,
	}
}

// Stages holds the data for all stages in the deployment process.
type Stages struct {
	Init        *InitStageData
	PreDeploy   *PreDeployStageData
	Deploy      *DeployStageData
	Destroy     *DestroyStageData
	PostDeploy  *PostDeployStageData
	PostDestroy *PostDestroyStageData
	Cleanup     *CleanupStageData
}

// RepositoryData holds information about the triggering repository.
type RepositoryData struct {
	Source       types2.SourceType // Source backend used for this deployment (git or oci)
	SourceUrl    string            // Repository or OCI artifact URL (e.g., "https://github.com/user/my-repo.git" or "ghcr.io/org/repo:tag")
	Name         string            // Repository name (e.g., "user/my-repo")
	PathInternal string            // Path to the repository inside the container
	PathExternal string            // Path to the repository on the host machine
	Git          *git.Repository   // Git repository instance
	Revision     string            // Resolved immutable revision (commit SHA or digest)
	OCITrusted   bool              // True when the OCI artifact passed trust-policy verification before reconciliation/cleanup
}

// Docker holds the Docker CLI and client instances along with the data mount point.
type Docker struct {
	Cmd            command.Cli
	DataMountPoint container.MountPoint
	Project        *types.Project
	SwarmMode      bool
	SwarmAvailable bool
}

// DeploymentState holds the dynamic state information during the deployment process.
type DeploymentState struct {
	changedServices      []docker.Change
	imageChangedServices []string // services whose image moved: digest drift under force_image_pull, otherwise a changed image reference
	ignoredInfo          docker.IgnoredInfo
	DeployedCommit       string // previously-deployed commit SHA, carried to post-deploy for the changelog
}

// changedServiceNames flattens the detected changes and image digest drifts into a unique list of service names.
func (d *DeploymentState) changedServiceNames() []string {
	if d == nil {
		return nil
	}

	var names []string
	for _, change := range d.changedServices {
		names = append(names, change.Services...)
	}

	names = append(names, d.imageChangedServices...)

	names = slice.Unique(names)
	slices.Sort(names)

	return names
}

// StageManager is the main structure that holds the logger and stage data.
type StageManager struct {
	Stages            *Stages
	Log               *slog.Logger
	JobID             string            // Unique identifier for the job
	JobTrigger        JobTrigger        // Trigger type for the job (e.g., "webhook", "poll")
	NotifyFailureFunc NotifyFailureFunc // Function to call on failure
	AppConfig         *app.Config
	DeployConfig      *deploy.Config
	DeployState       *DeploymentState
	Docker            *Docker
	Payload           *webhook.ParsedPayload
	Repository        *RepositoryData
	SecretProvider    secretprovider.SecretProvider
	Metadata          notification.Metadata // Notification metadata (may include reconciliation event info)
}

type NotifyFailureFunc func(log *slog.Logger, err error, metadata notification.Metadata)

// Dependencies holds the stable services shared by every StageManager run in a process:
// application configuration, the optional secret provider used to resolve external secret
// references, and the callback invoked when a deployment fails.
type Dependencies struct {
	AppConfig         *app.Config `validate:"required,nostructlevel"`
	SecretProvider    secretprovider.SecretProvider
	NotifyFailureFunc NotifyFailureFunc
}

// RunInput holds the per-deployment input for a single StageManager run: the job identity and
// trigger, logger, repository data, Docker CLI/data mount point, the parsed webhook payload
// (may be nil for non-webhook triggers), the resolved deploy config, and notification metadata.
type RunInput struct {
	Log          *slog.Logger `validate:"required,nostructlevel"`
	JobID        string
	JobTrigger   JobTrigger      `validate:"required,oneof=webhook poll"`
	Repository   *RepositoryData `validate:"required,nostructlevel"`
	Docker       *Docker         `validate:"required,nostructlevel"`
	Payload      *webhook.ParsedPayload
	DeployConfig *deploy.Config `validate:"required,nostructlevel"`
	Metadata     notification.Metadata
}

// NewStageManager validates dependencies and run, then creates and initializes a new
// StageManager instance for managing stages.
func NewStageManager(dependencies Dependencies, run RunInput) (*StageManager, error) {
	if err := validation.Validate(dependencies); err != nil {
		return nil, fmt.Errorf("validate stage dependencies: %w", err)
	}

	if err := validation.Validate(run); err != nil {
		return nil, fmt.Errorf("validate stage run input: %w", err)
	}

	return &StageManager{
		Log:               run.Log.With(),
		JobID:             run.JobID,
		JobTrigger:        run.JobTrigger,
		NotifyFailureFunc: dependencies.NotifyFailureFunc,
		AppConfig:         dependencies.AppConfig,
		DeployConfig:      run.DeployConfig,
		DeployState:       &DeploymentState{},
		Docker:            run.Docker,
		Payload:           run.Payload,
		Repository:        run.Repository,
		SecretProvider:    dependencies.SecretProvider,
		Metadata:          run.Metadata,
		Stages: &Stages{
			Init: &InitStageData{
				MetaData: NewMetaData(StageInit),
			},
			PreDeploy: &PreDeployStageData{
				MetaData: NewMetaData(StagePreDeploy),
			},
			Deploy: &DeployStageData{
				MetaData: NewMetaData(StageDeploy),
			},
			Destroy: &DestroyStageData{
				MetaData: NewMetaData(StageDestroy),
			},
			PostDeploy: &PostDeployStageData{
				MetaData: NewMetaData(StagePostDeploy),
			},
			PostDestroy: &PostDestroyStageData{
				MetaData: NewMetaData(StagePostDestroy),
			},
			Cleanup: &CleanupStageData{
				MetaData: NewMetaData(StageCleanup),
			},
		},
	}, nil
}

// GetStageMetaData retrieves the metadata for the specified stage.
func (s *StageManager) GetStageMetaData(stageName StageName) (*MetaData, error) {
	switch stageName {
	case StageInit:
		return &s.Stages.Init.MetaData, nil
	case StagePreDeploy:
		return &s.Stages.PreDeploy.MetaData, nil
	case StageDeploy:
		return &s.Stages.Deploy.MetaData, nil
	case StageDestroy:
		return &s.Stages.Destroy.MetaData, nil
	case StagePostDeploy:
		return &s.Stages.PostDeploy.MetaData, nil
	case StagePostDestroy:
		return &s.Stages.PostDestroy.MetaData, nil
	case StageCleanup:
		return &s.Stages.Cleanup.MetaData, nil
	default:
		return nil, errors.New("unknown stage name")
	}
}

// NotifyFailure sends a failure notification using the provided NotifyFailureFunc
// and returns notifyErr marked as already reported, so the caller that receives
// it does not notify about the same failure a second time. Without a
// NotifyFailureFunc nothing is sent and the error is returned unchanged.
func (s *StageManager) NotifyFailure(notifyErr error) error {
	var (
		latestCommit string
		commitErr    error
		commitSha    string
	)

	if s.NotifyFailureFunc != nil {
		if s.Repository.Git != nil {
			latestCommit, commitErr = gitInternal.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
			if commitErr != nil {
				latestCommit = ""
			}

			commitSha, commitErr = gitInternal.GetShortestUniqueCommitHash(s.Repository.Git, latestCommit, gitInternal.DefaultShortSHALength)
			if commitErr != nil {
				commitSha = latestCommit
			}
		}

		if s.Repository.Git == nil {
			commitSha = strings.TrimSpace(s.Repository.Revision)
		}

		revision := notification.GetRevision(s.DeployConfig.Reference, commitSha)

		metadata := s.Metadata
		metadata.Repository = s.Repository.Name
		metadata.Stack = s.DeployConfig.Name
		metadata.Context = s.DeployConfig.Context
		metadata.Target = s.DeployConfig.Internal.ConfigTarget
		metadata.Revision = revision
		metadata.JobID = s.JobID
		metadata.ChangedServices = s.DeployState.changedServiceNames()

		if !s.Stages.Init.StartedAt.IsZero() {
			metadata.Duration = time.Since(s.Stages.Init.StartedAt).Truncate(time.Millisecond)
		}

		s.NotifyFailureFunc(s.Log, notifyErr, metadata)

		return notification.MarkNotified(notifyErr)
	}

	return notifyErr
}

func (s *StageManager) NotifyDeploymentStarted() error {
	var (
		latestCommit string
		commitErr    error
		commitSha    string
	)

	if s.Repository.Git != nil {
		latestCommit, commitErr = gitInternal.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
		if commitErr == nil {
			commitSha, commitErr = gitInternal.GetShortestUniqueCommitHash(s.Repository.Git, latestCommit, gitInternal.DefaultShortSHALength)
			if commitErr != nil {
				commitSha = latestCommit
			}
		}
	}

	if s.Repository.Git == nil {
		commitSha = strings.TrimSpace(s.Repository.Revision)
	}

	revision := notification.GetRevision(s.DeployConfig.Reference, commitSha)

	metadata := s.Metadata
	metadata.Repository = s.Repository.Name
	metadata.Stack = s.DeployConfig.Name
	metadata.Context = s.DeployConfig.Context
	metadata.Target = s.DeployConfig.Internal.ConfigTarget
	metadata.Revision = revision
	metadata.JobID = s.JobID
	metadata.ChangedServices = s.DeployState.changedServiceNames()

	return notification.Send(
		notification.Info,
		"Deployment started",
		"Starting deployment of stack "+s.DeployConfig.Name,
		metadata,
	)
}

// resolveCommitSHA returns the full commit SHA for the current deployment.
// For webhook triggers the SHA is taken directly from the payload; for poll
// triggers it is resolved from the cloned repository after the init stage.
func (s *StageManager) resolveCommitSHA() string {
	if s.Repository.Source == types2.SourceTypeOCI {
		return "" // OCI digests are not git commit SHAs
	}

	// Prefer the full SHA from the local git repository when available.
	if s.Repository.Git != nil {
		sha, err := gitInternal.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
		if err == nil && strings.TrimSpace(sha) != "" {
			return strings.TrimSpace(sha)
		}
	}

	// Fall back to the SHA carried in the webhook payload.
	if s.Payload != nil {
		sha := strings.TrimSpace(s.Payload.CommitSHAString())
		if sha != "" && s.Payload.CommitSHA != plumbing.ZeroHash {
			return sha
		}
	}

	return strings.TrimSpace(s.Repository.Revision)
}

func (s *StageManager) resolveCommitStatusContext() string {
	return commitstatus.ContextForStack(s.DeployConfig.Internal.ConfigTarget, s.DeployConfig.Name)
}

func (s *StageManager) resolveCommitStatusRequest() (commitstatus.Provider, string, string, string, string, string, string, bool) {
	if !s.AppConfig.GitCommitStatus {
		return commitstatus.ProviderAuto, "", "", "", "", "", "", false
	}

	if s.Repository.Source == types2.SourceTypeOCI {
		return commitstatus.ProviderAuto, "", "", "", "", "", "", false
	}

	commitSHA := s.resolveCommitSHA()
	if commitSHA == "" {
		s.Log.Debug("skipping commit status: no commit SHA available")

		return commitstatus.ProviderAuto, "", "", "", "", "", "", false
	}

	resolved := gitInternal.ResolveAuthConfig(s.Repository.SourceUrl, "", "", "")

	token, err := gitInternal.ResolveHTTPToken(s.Repository.SourceUrl, resolved)
	if err != nil {
		s.Log.Warn("failed to resolve commit status token", slog.String("error", err.Error()))
	}

	if token == "" {
		token = s.AppConfig.GitAccessToken
	}

	if token == "" {
		s.Log.Debug("skipping commit status: no access token configured")

		return commitstatus.ProviderAuto, "", "", "", "", "", "", false
	}

	repoURL := ""
	repoFullName := ""

	if s.Payload != nil {
		repoURL = strings.TrimSpace(s.Payload.WebURL)
		repoFullName = strings.TrimSpace(s.Payload.FullName)
	}

	if repoURL == "" {
		repoURL = s.Repository.SourceUrl
	}

	if repoFullName == "" {
		repoFullName = gitInternal.GetFullName(repoURL)
	}

	provider, _ := commitstatus.ParseProvider(s.AppConfig.GitScmProvider)

	return provider, string(s.AppConfig.GitScmApiUrl), repoURL, repoFullName, commitSHA, token, s.resolveCommitStatusContext(), true
}

func (s *StageManager) GetCurrentCommitStatus(ctx context.Context) (commitstatus.Status, bool) {
	provider, apiBaseURL, repoURL, repoFullName, commitSHA, token, contextName, ok := s.resolveCommitStatusRequest()
	if !ok {
		return commitstatus.Status{}, false
	}

	s.Log.Debug("getting commit status",
		slog.String("provider", string(provider)),
		slog.String("repository", repoFullName),
		slog.String("commit_sha", commitSHA),
		slog.String("context", contextName),
	)

	status, found, err := commitstatus.Get(ctx, provider, apiBaseURL, repoURL, repoFullName, commitSHA, token, contextName)
	if err != nil {
		s.Log.Warn("failed to get commit status", slog.String("error", err.Error()))
		return commitstatus.Status{}, false
	}

	if !found {
		s.Log.Debug("no commit status found",
			slog.String("provider", string(provider)),
			slog.String("repository", repoFullName),
			slog.String("commit_sha", commitSHA),
			slog.String("context", contextName),
		)
	}

	return status, found
}

// PostCommitStatus posts a commit status to the source Git provider.
// It is a no-op when GIT_COMMIT_STATUS is disabled, when the source is OCI,
// or when no access token / commit SHA is available.
// Errors are logged as warnings so they never block a deployment.
func (s *StageManager) PostCommitStatus(ctx context.Context, state commitstatus.State, description string) {
	provider, apiBaseURL, repoURL, repoFullName, commitSHA, token, contextName, ok := s.resolveCommitStatusRequest()
	if !ok {
		return
	}

	s.Log.Debug("posting commit status",
		slog.String("provider", string(provider)),
		slog.String("repository", repoFullName),
		slog.String("commit_sha", commitSHA),
		slog.String("context", contextName),
		slog.String("state", string(state)),
		slog.String("description", description),
	)

	err := commitstatus.Post(ctx, provider, apiBaseURL, repoURL, repoFullName, commitSHA, token, commitstatus.Status{
		State:       state,
		Description: description,
		Context:     contextName,
	})
	if err != nil {
		s.Log.Warn("failed to post commit status", slog.String("error", err.Error()))
	}
}
