package stages

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/kimdre/doco-cd/internal/docker"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestStageRecordsDeploymentFailure(t *testing.T) {
	t.Parallel()

	want := map[StageName]bool{
		StageInit:        false,
		StagePreDeploy:   false,
		StageDeploy:      true,
		StagePostDeploy:  true,
		StageCleanup:     true,
		StageDestroy:     false,
		StagePostDestroy: false,
	}

	for stageName, wantRecord := range want {
		if got := stageRecordsDeploymentFailure(stageName); got != wantRecord {
			t.Errorf("stageRecordsDeploymentFailure(%q) = %v, want %v", stageName, got, wantRecord)
		}
	}
}

func TestRecordAndClearDeploymentFailure(t *testing.T) {
	t.Parallel()

	sm := newTestStageManager()
	sm.AppConfig.DataMountPath = t.TempDir()
	sm.DeployConfig.Name = "app"
	sm.Repository.Revision = "573a16e"

	sm.recordDeploymentFailure(discardLog(), StageDeploy, errors.New("hook exited with status 1"))

	failure, ok := sm.lastDeploymentFailure()
	if !ok {
		t.Fatal("expected a recorded failure after a failed deploy stage")
	}

	if failure.Stage != string(StageDeploy) || failure.CommitSHA != "573a16e" {
		t.Fatalf("unexpected failure record: %+v", failure)
	}

	sm.clearDeploymentFailure(discardLog())

	if _, ok := sm.lastDeploymentFailure(); ok {
		t.Fatal("expected no failure record after clearing")
	}
}

func TestRecordDeploymentFailureSkipsPreDeployStages(t *testing.T) {
	t.Parallel()

	sm := newTestStageManager()
	sm.AppConfig.DataMountPath = t.TempDir()
	sm.DeployConfig.Name = "app"

	sm.recordDeploymentFailure(discardLog(), StageInit, errors.New("clone failed"))
	sm.recordDeploymentFailure(discardLog(), StagePreDeploy, errors.New("status lookup failed"))

	if _, ok := sm.lastDeploymentFailure(); ok {
		t.Fatal("init and pre-deploy failures must not be recorded, they retry naturally")
	}
}

func TestRecordDeploymentFailureSkipsDestroy(t *testing.T) {
	t.Parallel()

	sm := newTestStageManager()
	sm.AppConfig.DataMountPath = t.TempDir()
	sm.DeployConfig.Name = "app"
	sm.DeployConfig.Destroy.Enabled = true

	sm.recordDeploymentFailure(discardLog(), StageDestroy, errors.New("destroy failed"))

	if _, ok := sm.lastDeploymentFailure(); ok {
		t.Fatal("destroy failures must not be recorded")
	}
}

func TestDeploymentFailureHelpersWithoutDataPath(t *testing.T) {
	t.Parallel()

	sm := newTestStageManager() // AppConfig has no DataMountPath

	sm.recordDeploymentFailure(discardLog(), StageDeploy, errors.New("boom"))
	sm.clearDeploymentFailure(discardLog())

	if _, ok := sm.lastDeploymentFailure(); ok {
		t.Fatal("expected no failure record without a data mount path")
	}

	// direct store access must also see nothing
	if _, ok := docker.GetDeploymentFailure("", sm.Repository.Name, sm.DeployConfig.Name); ok {
		t.Fatal("expected no marker written without a data mount path")
	}
}
