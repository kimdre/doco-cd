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
	"github.com/kimdre/doco-cd/internal/common/lifecycle"
	"github.com/kimdre/doco-cd/internal/common/types/slice"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/migration"
	"github.com/kimdre/doco-cd/internal/notification"

	gitInternal "github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
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
	Source            types2.SourceType // Source backend used for this deployment (git or oci)
	SourceUrl         string            // Repository or OCI artifact URL used for the deployment
	ConfigSourceUrl   string            // Resolved URL of the repository or artifact containing the deploy config
	Name              string            // Repository name (e.g., "user/my-repo")
	PathInternal      string            // Path to the repository inside the container
	PathExternal      string            // Path to the repository on the host machine
	Git               *git.Repository   // Git repository instance
	MirrorDir         string            // Path of Git's bare mirror clone backing Git; empty for OCI sources
	Revision          string            // Resolved immutable revision (commit SHA or digest)
	ResolvedReference string            // Reference that Revision/MirrorDir were resolved against (e.g., the branch/tag from the triggering job); empty for OCI sources
	ConfigRevision    string            // Immutable revision containing the deploy config
	ConfigPath        string            // Host path to the config source artifact
	OCITrusted        bool              // True when the OCI artifact passed trust-policy verification before reconciliation/cleanup
}

// SchedulerStopHolds reports whether a Compose service is currently held
// stopped by doco-cd's own job scheduler, i.e. a scheduled job listed it in
// cd.doco.job.stop_services and has not restarted it yet (the post-release
// grace period counts as held too).
//
// Holds are only registered for Compose-mode jobs, keyed by Docker context,
// Compose project and service name.
type SchedulerStopHolds interface {
	IsSchedulerStopHeld(contextName, project, service string) bool
}

// Docker holds the Docker CLI and client instances along with the data mount point.
type Docker struct {
	Cmd            command.Cli
	DataMountPoint container.MountPoint
	Project        *types.Project
	ProjectHash    string
	SwarmMode      bool
	SwarmAvailable bool
}

// DeploymentState holds the dynamic state information during the deployment process.
type DeploymentState struct {
	changedServices      []docker.Change
	imageChangedServices []string // services whose image moved: digest drift under force_image_pull, otherwise a changed image reference
	ignoredInfo          docker.IgnoredInfo
	modeMigrationNeeded  bool
	DeployedCommit       string // previously-deployed commit SHA, carried to post-deploy for the changelog
	latestCommit         string // current commit SHA, resolved during pre-deploy for reuse by deploy
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
	Stages         *Stages
	Log            *slog.Logger
	JobID          string     // Unique identifier for the job
	JobTrigger     JobTrigger // Trigger type for the job (e.g., "webhook", "poll")
	AppConfig      *app.Config
	DeployConfig   *deploy.Config
	DeployState    *DeploymentState
	Docker         *Docker
	Payload        *webhook.ParsedPayload
	Repository     *RepositoryData
	GitChanges     *GitChangeCache
	GitAncestry    *GitAncestryCache
	SecretProvider secretprovider.SecretProvider
	Notifier       notification.Sender
	Metadata       notification.Metadata // Notification metadata (may include reconciliation event info)
	// SchedulerHolds is optional; a nil value means no scheduler stop holds are tracked.
	SchedulerHolds SchedulerStopHolds
	// Contexts is the Docker context registry used by the cleanup stage to check whether legacy
	// on-disk leftovers are still referenced by a running container in any configured context.
	// A nil value disables the retry (the cleanup stage then only logs and continues).
	Contexts *docker.ContextRegistry
	// LeftoverTracker remembers repository directories already confirmed free of legacy
	// leftovers, so the cleanup stage can skip redundant checks for them. A nil value disables
	// the short-circuit (every run is checked from scratch).
	LeftoverTracker *migration.LeftoverTracker
	releaseGCLock   func()
}

// Dependencies holds the stable services shared by every StageManager run in a process:
// application configuration, the optional secret provider used to resolve external secret
// references, and the notifier used for deployment lifecycle messages.
type Dependencies struct {
	AppConfig      *app.Config                   `validate:"required,nostructlevel"`
	SecretProvider secretprovider.SecretProvider `validate:"omitempty,nostructlevel"`
	Notifier       notification.Sender           `validate:"required,nostructlevel"`
	// SchedulerHolds lets the pre-deploy stage ask whether a service is intentionally stopped by a running scheduled job.
	// A nil value disables the check. nostructlevel keeps the validator from recursing into the
	// concrete implementation (typically *reconciliation.Manager), which has
	// its own concurrently-locked internal state and races under -race if walked via reflection.
	SchedulerHolds SchedulerStopHolds `validate:"omitempty,nostructlevel"`
	// Contexts and LeftoverTracker are used by the cleanup stage to retry removal of legacy
	// on-disk leftovers left behind after migration (see internal/migration). Both are optional;
	// a nil Contexts disables the retry entirely, and a nil LeftoverTracker just disables the
	// in-memory short-circuit for repositories already confirmed clean.
	Contexts        *docker.ContextRegistry `validate:"omitempty,nostructlevel"`
	LeftoverTracker *migration.LeftoverTracker
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
	GitChanges   *GitChangeCache
	GitAncestry  *GitAncestryCache
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
		Log:             run.Log.With(),
		JobID:           run.JobID,
		JobTrigger:      run.JobTrigger,
		AppConfig:       dependencies.AppConfig,
		DeployConfig:    run.DeployConfig,
		DeployState:     &DeploymentState{},
		Docker:          run.Docker,
		Payload:         run.Payload,
		Repository:      run.Repository,
		GitChanges:      run.GitChanges,
		GitAncestry:     run.GitAncestry,
		SecretProvider:  dependencies.SecretProvider,
		Notifier:        dependencies.Notifier,
		SchedulerHolds:  dependencies.SchedulerHolds,
		Metadata:        run.Metadata,
		Contexts:        dependencies.Contexts,
		LeftoverTracker: dependencies.LeftoverTracker,
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

// acquireMirrorReadLock takes a shared lock on the repository's bare mirror
// directory for the duration of a read-only Git ref lookup (GetLatestCommit,
// GetChangedFilesBetweenCommits, GetCommitsBetween, ...). It excludes a
// concurrent stack's mirror fetch (which takes the matching exclusive lock
// in git.CloneOrUpdateBareMirror/GitStore.Publish) so a ref read never
// observes the mirror mid-write. It is a no-op when the mirror path is
// unknown, e.g. for OCI sources or before stage 1 has resolved it.
func (s *StageManager) acquireMirrorReadLock() func() {
	if s.Repository.MirrorDir == "" {
		return func() {}
	}

	return sourcecache.AcquireSharedPathLock(s.Repository.MirrorDir)
}

// NotifyFailure sends a failure notification and returns notifyErr marked as already
// reported, so the caller does not notify about the same failure a second time.
func (s *StageManager) NotifyFailure(notifyErr error) error {
	var (
		latestCommit string
		commitErr    error
		commitSha    string
	)

	if s.Repository.Git != nil {
		unlock := s.acquireMirrorReadLock()

		latestCommit, commitErr = gitInternal.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
		if commitErr != nil {
			latestCommit = ""
		}

		commitSha, commitErr = gitInternal.GetShortestUniqueCommitHash(s.Repository.Git, latestCommit, gitInternal.DefaultShortSHALength)
		if commitErr != nil {
			commitSha = latestCommit
		}

		unlock()
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

	go func() {
		if err := s.Notifier.Send(notification.Failure, "Deployment Failed", notifyErr.Error(), metadata); err != nil {
			s.Log.Error("failed to send notification", logger.ErrAttr(err))
		}
	}()

	s.Log.Error("deployment failed",
		slog.String("stack", metadata.Stack),
		logger.ErrAttr(notifyErr))

	return notification.MarkNotified(notifyErr)
}

func (s *StageManager) NotifyDeploymentStarted() error {
	var (
		latestCommit string
		commitErr    error
		commitSha    string
	)

	if s.Repository.Git != nil {
		unlock := s.acquireMirrorReadLock()

		latestCommit, commitErr = gitInternal.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
		if commitErr == nil {
			commitSha, commitErr = gitInternal.GetShortestUniqueCommitHash(s.Repository.Git, latestCommit, gitInternal.DefaultShortSHALength)
			if commitErr != nil {
				commitSha = latestCommit
			}
		}

		unlock()
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

	return s.Notifier.Send(
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
		unlock := s.acquireMirrorReadLock()
		sha, err := gitInternal.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)

		unlock()

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

func (s *StageManager) resolveCommitStatusRequest() (commitstatus.Request, bool) {
	repoURL := ""
	repoFullName := ""

	if s.Payload != nil {
		repoURL = s.Payload.WebURL
		repoFullName = s.Payload.FullName
	}

	return commitstatus.ResolveRequest(s.Log, commitstatus.RequestParams{
		Enabled:          s.AppConfig.GitCommitStatus,
		SourceIsGit:      s.Repository.Source != types2.SourceTypeOCI,
		SourceURL:        s.Repository.SourceUrl,
		CommitSHA:        s.resolveCommitSHA(),
		PayloadWebURL:    repoURL,
		PayloadFullName:  repoFullName,
		ProviderOverride: s.AppConfig.GitScmProvider,
		APIBaseURL:       string(s.AppConfig.GitScmApiUrl),
		AccessToken:      s.AppConfig.GitAccessToken,
		ContextName:      s.resolveCommitStatusContext(),
	})
}

func (s *StageManager) GetCurrentCommitStatus(ctx context.Context) (commitstatus.Status, bool) {
	req, ok := s.resolveCommitStatusRequest()
	if !ok {
		return commitstatus.Status{}, false
	}

	s.Log.Debug("getting commit status",
		slog.String("provider", string(req.Provider)),
		slog.String("repository", req.RepoFullName),
		slog.String("commit_sha", req.CommitSHA),
		slog.String("context", req.Context),
	)

	status, found, err := req.Get(ctx)
	if err != nil {
		s.Log.Warn("failed to get commit status", slog.String("error", err.Error()))
		return commitstatus.Status{}, false
	}

	if !found {
		s.Log.Debug("no commit status found",
			slog.String("provider", string(req.Provider)),
			slog.String("repository", req.RepoFullName),
			slog.String("commit_sha", req.CommitSHA),
			slog.String("context", req.Context),
		)
	}

	return status, found
}

// PostCommitStatus posts a commit status to the source Git provider.
// It is a no-op when GIT_COMMIT_STATUS is disabled, when the source is OCI,
// or when no access token / commit SHA is available.
// Errors are logged as warnings so they never block a deployment.
func (s *StageManager) PostCommitStatus(ctx context.Context, state commitstatus.State, description string) {
	req, ok := s.resolveCommitStatusRequest()
	if !ok {
		return
	}

	s.Log.Debug("posting commit status",
		slog.String("provider", string(req.Provider)),
		slog.String("repository", req.RepoFullName),
		slog.String("commit_sha", req.CommitSHA),
		slog.String("context", req.Context),
		slog.String("state", string(state)),
		slog.String("description", description),
	)

	err := req.Post(ctx, commitstatus.Status{
		State:       state,
		Description: description,
	})
	if err != nil {
		if lifecycle.IsCanceled(err) {
			s.Log.Debug("skipped commit status during application shutdown", slog.String("error", err.Error()))

			return
		}

		s.Log.Warn("failed to post commit status", slog.String("error", err.Error()))
	}
}

// sourceLockKey returns the key used to serialize in-place mutation of this
// deployment's published artifact directory: LoadCompose decrypts
// SOPS-encrypted files in place there, so two deployments landing on the
// same revision (the same artifact directory) must agree on this key to
// exclude each other. It is a different, finer-grained key than the one
// source.Prepare locks (the repository's top-level directory, guarding
// against a concurrent destroy rather than against decrypt races).
func (s *StageManager) sourceLockKey() string {
	if s.Repository == nil {
		return ""
	}

	if s.Repository.PathInternal != "" {
		return s.Repository.PathInternal
	}

	return s.Repository.PathExternal
}

// migrationSource returns the source identity used to prove that previous-mode
// resources belong to this deployment. Pre-deploy inspection and the deploy
// stage's actual migration must resolve it identically, otherwise ownership
// validation could reject a migration the inspection already approved.
func (s *StageManager) migrationSource() string {
	if s.Payload != nil {
		if fullName := strings.TrimSpace(s.Payload.FullName); fullName != "" {
			return fullName
		}
	}

	if s.Repository == nil {
		return ""
	}

	return s.Repository.SourceUrl
}
