package stages

import (
	"log/slog"
	"time"
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

// StageManager is the main structure that holds the logger and stage data.
type StageManager struct {
	Stages     *Stages
	Log        *slog.Logger
	JobID      string            // Unique identifier for the job
	FailFunc   func(args ...any) // Function to call on failure
	NotifyFunc func(args ...any) // Function to call for notifications
}

// NewStageManager creates and initializes a new StageManager instance for managing stages.ß
func NewStageManager(log *slog.Logger, jobID string, failFunc func(args ...any), notifyFunc func(args ...any)) *StageManager {
	return &StageManager{
		Log:        log,
		JobID:      jobID,
		FailFunc:   failFunc,
		NotifyFunc: notifyFunc,
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
