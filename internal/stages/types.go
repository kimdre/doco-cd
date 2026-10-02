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
	"github.com/kimdre/doco-cd/internal/selfupdate"

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

	// ErrSyncWindowBlocked is returned when a sync window deferred the
	// deployment. It wraps ErrSkipDeployment, because a deferred deployment is
	// an intentional no-op and not a failure.
	ErrSyncWindowBlocked = fmt.Errorf("deferred by sync window: %w", ErrSkipDeployment)
)

// SyncWindowBlockedError describes stacks whose deployment was deferred by
// sync windows. It unwraps to ErrSyncWindowBlocked.
type SyncWindowBlockedError struct {
	Stacks  []string // Stacks are the names of the deferred stacks.
	Windows []string // Windows are the names of the windows that deferred them.
	// NextOpen is the earliest time at which one of the deferred stacks may be
	// deployed again. It is zero when unknown.
	NextOpen time.Time
}

func (e *SyncWindowBlockedError) Error() string {
	msg := fmt.Sprintf("deployment of %s deferred by sync window %s",
		strings.Join(e.Stacks, ", "), strings.Join(e.Windows, ", "))
	if !e.NextOpen.IsZero() {
		msg += " until " + e.NextOpen.Format(time.RFC3339)
	}

	return msg
}

func (e *SyncWindowBlockedError) Unwrap() error {
	return ErrSyncWindowBlocked
}

// SyncWindowCommitStatusDescription returns the pending commit status
// description of a deployment deferred until nextOpen.
func SyncWindowCommitStatusDescription(nextOpen time.Time) string {
	if nextOpen.IsZero() {
		return "Deferred by sync window"
	}

	return "Deferred by sync window until " + nextOpen.Format(time.RFC3339)
}

// MergeSyncWindowBlocked combines per-stack sync window errors into one,
// keeping the earliest known NextOpen. It returns nil for no errors.
func MergeSyncWindowBlocked(blocked []*SyncWindowBlockedError) *SyncWindowBlockedError {
	if len(blocked) == 0 {
		return nil
	}

	merged := &SyncWindowBlockedError{}

	for _, b := range blocked {
		if b == nil {
			continue
		}

		for _, stack := range b.Stacks {
			if !slices.Contains(merged.Stacks, stack) {
				merged.Stacks = append(merged.Stacks, stack)
			}
		}

		for _, window := range b.Windows {
			if !slices.Contains(merged.Windows, window) {
				merged.Windows = append(merged.Windows, window)
			}
		}

		if !b.NextOpen.IsZero() && (merged.NextOpen.IsZero() || b.NextOpen.Before(merged.NextOpen)) {
			merged.NextOpen = b.NextOpen
		}
	}

	slices.Sort(merged.Stacks)
	slices.Sort(merged.Windows)

	return merged
}

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
	MirrorDir         string            // Path of the bare mirror clone backing git reads; empty for OCI sources
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
	ProjectSkips   *ProjectSkipCache
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
	// resolvedOwnReference is set by the init stage if it resolved the deploy
	// config's own reference instead of reusing the revision of the request.
	resolvedOwnReference bool
	commitStatusTarget   *commitstatus.Target
	// inProgressPosted is set once the deployment's "In Progress" commit
	// status was posted, which phase updates then refine.
	inProgressPosted bool
}

// ResolvedOwnReference reports whether the init stage resolved the deploy
// config's reference itself (because of its own reference, repository_url or
// git_depth) instead of reusing the revision of the request. Repository.Revision
// then holds the revision resolved for this stack.
func (s *StageManager) ResolvedOwnReference() bool {
	return s.resolvedOwnReference
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
	ProjectSkips *ProjectSkipCache
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
		ProjectSkips:    run.ProjectSkips,
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

// hasGitMirror reports whether this deployment has a bare mirror to read git
// history from. OCI sources never do, and neither does a deployment whose init
// stage has not resolved one yet.
func (s *StageManager) hasGitMirror() bool {
	return s.Repository.MirrorDir != ""
}

// withMirrorRead runs fn against a freshly opened handle on this deployment's bare
// mirror while holding the mirror's shared read lock.
//
// The handle deliberately does not outlive fn. Retaining one across lock regions is
// what crashed doco-cd: go-git caches a repository handle's packfile index map on
// first use and never refreshes it, so once a concurrent job fetches into the same
// mirror the retained handle enumerates a packfile it has no index for and
// segfaults inside packfile.GetByType. See git.WithMirrorRead for the full
// mechanism. Batch every read of one logical region into a single call so the
// region shares one handle and one lock acquisition.
func (s *StageManager) withMirrorRead(fn func(repo *git.Repository) error) error {
	return gitInternal.WithMirrorRead(s.Repository.MirrorDir, fn)
}

// mirrorRead is the value-returning form of StageManager.withMirrorRead.
func mirrorRead[T any](s *StageManager, fn func(repo *git.Repository) (T, error)) (T, error) {
	return gitInternal.MirrorRead(s.Repository.MirrorDir, fn)
}

// latestCommitFromMirror resolves the deploy config's reference to a commit SHA
// using a scoped mirror read.
func (s *StageManager) latestCommitFromMirror() (string, error) {
	return mirrorRead(s, func(repo *git.Repository) (string, error) {
		return gitInternal.GetLatestCommit(repo, s.DeployConfig.Reference)
	})
}

// notificationCommitSha resolves the short commit SHA shown in notifications.
// It always prefers the immutable revision published for this deployment over the
// mirror's moving branch, which another webhook may already have advanced.
func (s *StageManager) notificationCommitSha() string {
	fullSHA := strings.TrimSpace(s.Repository.Revision)
	if !s.hasGitMirror() {
		return fullSHA
	}

	commitSha, err := mirrorRead(s, func(repo *git.Repository) (string, error) {
		if fullSHA == "" {
			var resolveErr error

			fullSHA, resolveErr = gitInternal.GetLatestCommit(repo, s.DeployConfig.Reference)
			if resolveErr != nil {
				return "", resolveErr
			}
		}

		shortSha, shortErr := gitInternal.GetShortestUniqueCommitHash(repo, fullSHA, gitInternal.DefaultShortSHALength)
		if shortErr != nil {
			// Shortening is cosmetic: fall back to the full SHA rather than failing
			// the notification over it.
			return fullSHA, nil //nolint:nilerr // intentional degradation
		}

		return shortSha, nil
	})
	if err != nil {
		return fullSHA
	}

	return commitSha
}

// NotifyFailure sends a failure notification and returns notifyErr marked as already
// reported, so the caller does not notify about the same failure a second time.
func (s *StageManager) NotifyFailure(notifyErr error) error {
	commitSha := s.notificationCommitSha()

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
	commitSha := s.notificationCommitSha()

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

// revisionBelongsToStack reports whether Repository.Revision is the revision of
// this stack's own repository and reference. The request's revision is set before
// the init stage runs, but it was resolved for the job's reference, which a stack
// with its own reference or repository_url does not deploy until the init stage
// replaced it with the revision resolved for that stack.
func (s *StageManager) revisionBelongsToStack() bool {
	if s.resolvedOwnReference {
		return true
	}

	return s.DeployConfig.RepositoryUrl == "" &&
		resolvedReferenceMatches(s.Repository.ResolvedReference, s.DeployConfig.Reference)
}

// resolveCommitSHA returns the full commit SHA the commit status of this
// deployment belongs to. Once Repository.Revision is the stack's own revision,
// that immutable revision is used: the mirror's branch head may already have moved
// on with a later push, and reading it opens a mirror handle on every status.
// Otherwise, such as when the init stage failed before resolving the stack's own
// reference, the reference is resolved from the mirror, falling back to the SHA
// carried in the webhook payload and then to the request's revision.
func (s *StageManager) resolveCommitSHA() string {
	if s.Repository.Source == types2.SourceTypeOCI {
		return "" // OCI digests are not git commit SHAs
	}

	revision := strings.TrimSpace(s.Repository.Revision)
	if revision != "" && s.revisionBelongsToStack() {
		return revision
	}

	if s.hasGitMirror() {
		sha, err := s.latestCommitFromMirror()
		if err == nil && strings.TrimSpace(sha) != "" {
			return strings.TrimSpace(sha)
		}
	}

	if s.Payload != nil {
		sha := strings.TrimSpace(s.Payload.CommitSHAString())
		if sha != "" && s.Payload.CommitSHA != plumbing.ZeroHash {
			return sha
		}
	}

	return revision
}

func (s *StageManager) resolveCommitStatusContext() string {
	return commitstatus.ContextForStack(s.DeployConfig.Internal.ConfigTarget, s.DeployConfig.Name)
}

func (s *StageManager) commitStatusParams() commitstatus.RequestParams {
	repoURL := ""
	repoFullName := ""

	if s.Payload != nil {
		repoURL = s.Payload.WebURL
		repoFullName = s.Payload.FullName
	}

	enabled := s.AppConfig.GitCommitStatus
	sourceIsGit := s.Repository.Source != types2.SourceTypeOCI

	// The commit SHA may need a mirror read, which is wasted when no commit
	// status is posted.
	var commitSHA string
	if enabled && sourceIsGit {
		commitSHA = s.resolveCommitSHA()
	}

	return commitstatus.RequestParams{
		Enabled:          enabled,
		SourceIsGit:      sourceIsGit,
		SourceURL:        s.Repository.SourceUrl,
		CommitSHA:        commitSHA,
		PayloadWebURL:    repoURL,
		PayloadFullName:  repoFullName,
		ProviderOverride: s.AppConfig.GitScmProvider,
		APIBaseURL:       string(s.AppConfig.GitScmApiUrl),
		AccessToken:      s.AppConfig.GitAccessToken,
		ContextName:      s.resolveCommitStatusContext(),
		Target:           s.commitStatusTarget,
		Scope:            docker.NormalizeContextName(s.DeployConfig.Context),
	}
}

func (s *StageManager) resolveCommitStatusRequest() (commitstatus.Request, bool) {
	req, ok := commitstatus.ResolveRequest(s.Log, s.commitStatusParams())
	if ok {
		s.commitStatusTarget = req.Target
	}

	return req, ok
}

// selfUpdateCommitStatus returns the target of the commit status this
// deployment leaves pending, so a self-update can hand it to the process that
// resolves the handover. It is nil when the deployment posts no commit status.
func (s *StageManager) selfUpdateCommitStatus() *selfupdate.CommitStatusInfo {
	if s.DeployConfig.Destroy.Enabled {
		return nil
	}

	params := s.commitStatusParams()

	commitSHA := strings.TrimSpace(params.CommitSHA)
	if !params.Enabled || !params.SourceIsGit || commitSHA == "" {
		return nil
	}

	var startedAt time.Time
	if s.Stages != nil && s.Stages.Init != nil {
		startedAt = s.Stages.Init.StartedAt
	}

	info := &selfupdate.CommitStatusInfo{
		SourceURL: params.SourceURL,
		RepoURL:   strings.TrimSpace(params.PayloadWebURL),
		FullName:  strings.TrimSpace(params.PayloadFullName),
		CommitSHA: commitSHA,
		Context:   params.ContextName,
		StartedAt: startedAt,
	}

	if s.commitStatusTarget != nil && s.commitStatusTarget.Backend == commitstatus.BackendChecks {
		target := *s.commitStatusTarget
		info.Target = &target
	}

	return info
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
	s.PostCommitStatusWithOutcome(ctx, state, description, "")
}

func (s *StageManager) PostCommitStatusWithOutcome(ctx context.Context, state commitstatus.State, description string, outcome commitstatus.Outcome) {
	s.postCommitStatus(ctx, state, description, outcome)
}

// postCommitStatus posts a commit status like PostCommitStatusWithOutcome and
// reports whether it was posted.
func (s *StageManager) postCommitStatus(ctx context.Context, state commitstatus.State, description string, outcome commitstatus.Outcome) bool {
	req, ok := s.resolveCommitStatusRequest()
	if !ok {
		return false
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
		Outcome:     outcome,
		Description: description,
	})
	if err != nil {
		if lifecycle.IsCanceled(err) {
			s.Log.Debug("skipped commit status during application shutdown", slog.String("error", err.Error()))

			return false
		}

		s.Log.Warn("failed to post commit status", slog.String("error", err.Error()))

		return false
	}

	return true
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

// verifyMirrorReadable fails fast if the bare mirror backing a deployment cannot be
// opened, so the init stage reports an unreadable mirror instead of letting a later
// stage fail mid-deployment. The handle it opens is intentionally discarded: go-git
// caches a handle's packfile index on first use and never refreshes it, so a handle
// retained past this point would break as soon as another job fetched into the same
// mirror. Later stages open their own handle per read via StageManager.withMirrorRead.
func verifyMirrorReadable(mirrorDir string) error {
	err := gitInternal.WithMirrorRead(mirrorDir, func(*git.Repository) error {
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to open repository mirror: %w", err)
	}

	return nil
}
