package stages

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/go-git/go-git/v5"
	"github.com/moby/moby/api/types/container"

	types2 "github.com/kimdre/doco-cd/internal/config"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/hook"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"

	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/webhook"
)

var (
	ErrNotManagedByDocoCD = errors.New("stack is not managed by doco-cd")
	ErrDeploymentConflict = errors.New("another stack with the same name already exists and is not managed by this repository")
	ErrSkipDeployment     = errors.New("deployment skipped") // Special error to indicate deployment was skipped, not an actual failure/error
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
}

// Docker holds the Docker CLI and client instances along with the data mount point.
type Docker struct {
	Cmd            command.Cli
	DataMountPoint container.MountPoint
	Project        *types.Project
}

// DeploymentState holds the dynamic state information during the deployment process.
type DeploymentState struct {
	changedServices []docker.Change
	ignoredInfo     docker.IgnoredInfo
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
	SecretProvider    *secretprovider.SecretProvider
	Metadata          notification.Metadata // Notification metadata (may include reconciliation event info)
}

type NotifyFailureFunc func(log *slog.Logger, err error, metadata notification.Metadata)

// NewStageManager creates and initializes a new StageManager instance for managing stages.ß.
func NewStageManager(jobID string, jobTrigger JobTrigger, log *slog.Logger,
	failNotifyFunc NotifyFailureFunc,
	repoData *RepositoryData, dockerData *Docker, payload *webhook.ParsedPayload,
	appConfig *app.Config, deployConfig *deploy.Config,
	secretProvider *secretprovider.SecretProvider,
	metadata notification.Metadata,
) *StageManager {
	return &StageManager{
		Log:               log.With(),
		JobID:             jobID,
		JobTrigger:        jobTrigger,
		NotifyFailureFunc: failNotifyFunc,
		AppConfig:         appConfig,
		DeployConfig:      deployConfig,
		DeployState:       &DeploymentState{},
		Docker:            dockerData,
		Payload:           payload,
		Repository:        repoData,
		SecretProvider:    secretProvider,
		Metadata:          metadata,
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
	}
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
// and fires any configured on-failure hooks.
func (s *StageManager) NotifyFailure(ctx context.Context, notifyErr error) {
	var (
		latestCommit string
		commitErr    error
		commitSha    string
	)

	if s.Repository.Git != nil {
		latestCommit, commitErr = gitInternal.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
		if commitErr != nil {
			latestCommit = ""
		}

		commitSha, commitErr = gitInternal.GetShortestUniqueCommitHash(s.Repository.Git, latestCommit, gitInternal.DefaultShortSHALength)
		if commitErr != nil {
			commitSha = latestCommit
		}
	} else {
		commitSha = strings.TrimSpace(s.Repository.Revision)
	}

	revision := notification.GetRevision(s.DeployConfig.Reference, commitSha)

	if s.NotifyFailureFunc != nil {
		metadata := s.Metadata
		metadata.Repository = s.Repository.Name
		metadata.Stack = s.DeployConfig.Name
		metadata.Revision = revision
		metadata.JobID = s.JobID

		s.NotifyFailureFunc(s.Log, notifyErr, metadata)
	}

	s.fireHooks(ctx, s.DeployConfig.Hooks.OnFailure, "failure", revision, notifyErr.Error())
}

// changedServiceImages returns the resolved image references of the services that
// changed in this deployment, deduplicated and sorted. It returns nil when the
// compose project is unavailable (e.g. a failure before the project was loaded).
func (s *StageManager) changedServiceImages() []string {
	if s.Docker == nil || s.Docker.Project == nil {
		return nil
	}

	seen := make(map[string]struct{})

	var images []string

	for _, change := range s.DeployState.changedServices {
		for _, name := range change.Services {
			svc, ok := s.Docker.Project.Services[name]
			if !ok || svc.Image == "" {
				continue
			}

			if _, dup := seen[svc.Image]; dup {
				continue
			}

			seen[svc.Image] = struct{}{}
			images = append(images, svc.Image)
		}
	}

	slices.Sort(images)

	return images
}

// fireHooks delivers the configured webhook hooks for a lifecycle event.
// Hook failures are logged but never abort the deployment.
func (s *StageManager) fireHooks(ctx context.Context, webhooks []hook.Webhook, event, revision, errMsg string) {
	if len(webhooks) == 0 {
		return
	}

	payload := hook.Payload{
		Event:      event,
		Repository: s.Repository.Name,
		Stack:      s.DeployConfig.Name,
		Revision:   revision,
		JobID:      s.JobID,
		Images:     s.changedServiceImages(),
		Error:      errMsg,
	}

	s.Log.Info("sending deployment hooks",
		slog.String("event", event),
		slog.Int("count", len(webhooks)))

	for _, w := range webhooks {
		if err := hook.Send(ctx, w, payload); err != nil {
			s.Log.Error("failed to send deployment hook",
				slog.String("event", event),
				slog.String("url", w.URL),
				logger.ErrAttr(err))

			continue
		}

		s.Log.Info("sent deployment hook",
			slog.String("event", event),
			slog.String("url", w.URL))
	}
}
