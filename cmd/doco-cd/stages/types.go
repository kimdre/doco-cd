package stages

import (
	"log/slog"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

type StageName string

const (
	StageInit       StageName = "init"
	StagePreDeploy  StageName = "pre-deploy"
	StageDeploy     StageName = "deploy"
	StagePostDeploy StageName = "post-deploy"
	StageCleanup    StageName = "cleanup"
)

type StageResult string

type StageStatus string

const (
	StageStatusPending   StageStatus = "pending"
	StageStatusRunning   StageStatus = "running"
	StageStatusCompleted StageStatus = "completed"
	StageStatusFailed    StageStatus = "failed"
)

type MetaData struct {
	Name       StageName
	Status     StageStatus
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

// PostDeployStageData holds the configuration and data specific to the post-deployment stage.
type PostDeployStageData struct {
	MetaData
}

// CleanupStageData holds the configuration and data specific to the cleanup stage.
type CleanupStageData struct {
	MetaData
}

func NewMetaData(name StageName) MetaData {
	return MetaData{
		Name:   name,
		Status: StageStatusPending,
	}
}

// Stages holds the data for all stages in the deployment process.
type Stages struct {
	Init       *InitStageData
	PreDeploy  *PreDeployStageData
	Deploy     *DeployStageData
	PostDeploy *PostDeployStageData
	Cleanup    *CleanupStageData
}

// RepositoryData holds information about the triggering repository.
type RepositoryData struct {
	CloneURL string // Repository clone URL
	Name     string // Repository name
	Revision string // Branch, tag, or commit hash
}

// Docker holds the Docker CLI and client instances along with the data mount point.
type Docker struct {
	Cmd            command.Cli
	Client         *client.Client
	DataMountPoint container.MountPoint
}

// StageManager is the main structure that holds the logger and stage data.
type StageManager struct {
	Stages         *Stages
	Log            *slog.Logger
	JobID          string            // Unique identifier for the job
	FailFunc       func(args ...any) // Function to call on failure
	NotifyFunc     func(args ...any) // Function to call for notifications
	AppConfig      *config.AppConfig
	Docker         *Docker
	Repository     *RepositoryData
	SecretProvider *secretprovider.SecretProvider
}

// NewStageManager creates and initializes a new StageManager instance for managing stages.ß
func NewStageManager(jobID string, log *slog.Logger,
	failFunc func(args ...any), notifyFunc func(args ...any),
	repoData *RepositoryData, dockerData *Docker, appConfig *config.AppConfig, secretProvider *secretprovider.SecretProvider) *StageManager {
	return &StageManager{
		Log:            log.With(slog.String("job_id", jobID)),
		JobID:          jobID,
		FailFunc:       failFunc,
		NotifyFunc:     notifyFunc,
		AppConfig:      appConfig,
		Docker:         dockerData,
		Repository:     repoData,
		SecretProvider: secretProvider,
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
			PostDeploy: &PostDeployStageData{
				MetaData: NewMetaData(StagePostDeploy),
			},
			Cleanup: &CleanupStageData{
				MetaData: NewMetaData(StageCleanup),
			},
		},
	}
}

// Notify sends a notification using the provided NotifyFunc.
func (s *StageManager) Notify(args ...any) {
	if s.NotifyFunc != nil {
		s.NotifyFunc(args...)
	}
}

// Fail triggers a failure using the provided FailFunc.
func (s *StageManager) Fail(args ...any) {
	if s.FailFunc != nil {
		s.FailFunc(args...)
	}
}
