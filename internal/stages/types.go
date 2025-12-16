package stages

import (
	"errors"
	"log/slog"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/go-git/go-git/v5"

	"github.com/kimdre/doco-cd/internal/notification"

	"github.com/kimdre/doco-cd/internal/config"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
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
	CloneURL     config.HttpUrl  // Repository clone URL (e.g., "https://github.com/user/my-repo.git")
	Name         string          // Repository name (e.g., "user/my-repo")
	PathInternal string          // Path to the repository inside the container
	PathExternal string          // Path to the repository on the host machine
	Git          *git.Repository // Git repository instance
}

// Docker holds the Docker CLI and client instances along with the data mount point.
type Docker struct {
	Cmd            command.Cli
	Client         *client.Client
	DataMountPoint container.MountPoint
}

// DeploymentState holds the dynamic state information during the deployment process.
type DeploymentState struct {
	ChangedFiles    []gitInternal.ChangedFile
	SecretsChanged  bool
	ResolvedSecrets secrettypes.ResolvedSecrets
}

// StageManager is the main structure that holds the logger and stage data.
type StageManager struct {
	Stages            *Stages
	Log               *slog.Logger
	JobID             string                                          // Unique identifier for the job
	JobTrigger        JobTrigger                                      // Trigger type for the job (e.g., "webhook", "poll")
	NotifyFailureFunc func(err error, metadata notification.Metadata) // Function to call on failure
	AppConfig         *config.AppConfig
	DeployConfig      *config.DeployConfig
	DeployState       *DeploymentState
	Docker            *Docker
	Payload           *webhook.ParsedPayload
	Repository        *RepositoryData
	SecretProvider    *secretprovider.SecretProvider
}

// NewStageManager creates and initializes a new StageManager instance for managing stages.ß.
func NewStageManager(jobID string, jobTrigger JobTrigger, log *slog.Logger,
	failNotifyFunc func(err error, metadata notification.Metadata),
	repoData *RepositoryData, dockerData *Docker, payload *webhook.ParsedPayload,
	appConfig *config.AppConfig, deployConfig *config.DeployConfig,
	secretProvider *secretprovider.SecretProvider,
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
				MetaData: NewMetaData(StageDeploy),
			},
			PostDeploy: &PostDeployStageData{
				MetaData: NewMetaData(StagePostDeploy),
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

// NotifyFailure sends a failure notification using the provided NotifyFailureFunc.
func (s *StageManager) NotifyFailure(notifyErr error) {
	var (
		latestCommit string
		shortCommit  string
		commitErr    error
	)

	if s.NotifyFailureFunc != nil {
		if s.Repository.Git != nil {
			latestCommit, commitErr = gitInternal.GetLatestCommit(s.Repository.Git, s.DeployConfig.Reference)
			if commitErr != nil {
				latestCommit = ""
			}
		}

		shortCommit, commitErr = gitInternal.GetShortestUniqueCommitSHA(s.Repository.Git, latestCommit, gitInternal.DefaultShortSHALength)
		if commitErr == nil {
			shortCommit = latestCommit
		}

		revision := notification.GetRevision(s.DeployConfig.Reference, shortCommit)

		s.NotifyFailureFunc(notifyErr, notification.Metadata{
			Repository: s.Repository.Name,
			Stack:      s.DeployConfig.Name,
			Revision:   revision,
			JobID:      s.JobID,
		})
	}
}
