package stages

import (
	"errors"
	"testing"

	"github.com/kimdre/doco-cd/internal/docker"
)

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

// the failure store is process-global, every test uses its own stack name.
func newFailureTestManager(t *testing.T, stack string) *StageManager {
	t.Helper()

	sm := newTestStageManager()
	sm.DeployConfig.Name = stack
	sm.Repository.Revision = "573a16e"

	t.Cleanup(func() { docker.ClearDeploymentFailure(sm.Repository.Name, stack) })

	return sm
}

func TestRecordAndClearDeploymentFailure(t *testing.T) {
	t.Parallel()

	sm := newFailureTestManager(t, "app-record-clear")

	sm.recordDeploymentFailure(StageDeploy, errors.New("hook exited with status 1"))

	failure, ok := sm.lastDeploymentFailure()
	if !ok {
		t.Fatal("expected a recorded failure after a failed deploy stage")
	}

	if failure.Stage != string(StageDeploy) || failure.CommitSHA != "573a16e" {
		t.Fatalf("unexpected failure record: %+v", failure)
	}

	sm.clearDeploymentFailure()

	if _, ok := sm.lastDeploymentFailure(); ok {
		t.Fatal("expected no failure record after clearing")
	}
}

func TestRecordDeploymentFailureSkipsPreDeployStages(t *testing.T) {
	t.Parallel()

	sm := newFailureTestManager(t, "app-skip-predeploy")

	sm.recordDeploymentFailure(StageInit, errors.New("clone failed"))
	sm.recordDeploymentFailure(StagePreDeploy, errors.New("status lookup failed"))

	if _, ok := sm.lastDeploymentFailure(); ok {
		t.Fatal("init and pre-deploy failures must not be recorded, they retry naturally")
	}
}

func TestRecordDeploymentFailureSkipsDestroy(t *testing.T) {
	t.Parallel()

	sm := newFailureTestManager(t, "app-skip-destroy")
	sm.DeployConfig.Destroy.Enabled = true

	sm.recordDeploymentFailure(StageDestroy, errors.New("destroy failed"))

	if _, ok := sm.lastDeploymentFailure(); ok {
		t.Fatal("destroy failures must not be recorded")
	}
}
