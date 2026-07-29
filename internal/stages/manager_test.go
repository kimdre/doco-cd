package stages

import (
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/notification"
)

func newTestStageManager() *StageManager {
	return NewStageManager(
		"job-1",
		JobTriggerWebhook,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
		&RepositoryData{Name: "owner/repo"},
		&Docker{},
		nil,
		&app.Config{},
		&deploy.Config{},
		nil,
		notification.Metadata{},
	)
}

func TestNewMetaData(t *testing.T) {
	t.Parallel()

	meta := NewMetaData(StageDeploy)
	if meta.Name != StageDeploy {
		t.Fatalf("NewMetaData() name = %q, want %q", meta.Name, StageDeploy)
	}

	if !meta.StartedAt.IsZero() || !meta.FinishedAt.IsZero() {
		t.Fatal("NewMetaData() should zero timestamps")
	}
}

func TestNewStageManager(t *testing.T) {
	t.Parallel()

	sm := newTestStageManager()

	if sm.JobID != "job-1" || sm.JobTrigger != JobTriggerWebhook {
		t.Fatalf("NewStageManager() stored job metadata incorrectly: %#v", sm)
	}

	if sm.Stages == nil || sm.DeployState == nil {
		t.Fatal("NewStageManager() should initialize stages and deploy state")
	}

	if sm.Stages.Init.Name != StageInit || sm.Stages.Cleanup.Name != StageCleanup {
		t.Fatalf("NewStageManager() stage metadata names = %#v", sm.Stages)
	}
}

func TestStageManagerGetStageMetaData(t *testing.T) {
	t.Parallel()

	sm := newTestStageManager()

	tests := []StageName{StageInit, StagePreDeploy, StageDeploy, StageDestroy, StagePostDeploy, StagePostDestroy, StageCleanup}
	for _, stageName := range tests {
		t.Run(string(stageName), func(t *testing.T) {
			t.Parallel()

			meta, err := sm.GetStageMetaData(stageName)
			if err != nil {
				t.Fatalf("GetStageMetaData(%q) = %v", stageName, err)
			}

			if meta == nil || meta.Name != stageName {
				t.Fatalf("GetStageMetaData(%q) = %#v", stageName, meta)
			}
		})
	}

	if _, err := sm.GetStageMetaData(StageName("unknown")); err == nil {
		t.Fatal("GetStageMetaData(unknown) = nil, want error")
	}
}

func TestStageManagerNotifyFailureIncludesTarget(t *testing.T) {
	t.Parallel()

	sm := newTestStageManager()
	sm.Repository.Revision = "abc123"
	sm.DeployConfig.Name = "app"
	sm.DeployConfig.Context = "remote-vm"
	sm.DeployConfig.Reference = "main"
	sm.DeployConfig.Internal.ConfigTarget = "prod-vm"

	var got notification.Metadata

	sm.NotifyFailureFunc = func(_ *slog.Logger, _ error, metadata notification.Metadata) {
		got = metadata
	}

	sm.NotifyFailure(errors.New("boom"))

	if got.Target != "prod-vm" {
		t.Fatalf("expected target prod-vm, got %q", got.Target)
	}
}

func TestStageOrders(t *testing.T) {
	t.Parallel()

	sm := newTestStageManager()

	deployOrder := sm.GetDeployStageOrder()
	if want := []StageName{StageInit, StagePreDeploy, StageDeploy, StagePostDeploy, StageCleanup}; !slices.Equal(deployOrder.Order, want) {
		t.Fatalf("GetDeployStageOrder() order = %v, want %v", deployOrder.Order, want)
	}

	for _, stageName := range deployOrder.Order {
		if deployOrder.Funcs[stageName] == nil {
			t.Fatalf("GetDeployStageOrder() missing function for %q", stageName)
		}
	}

	destroyOrder := sm.GetDestroyStageOrder()
	if want := []StageName{StageInit, StageDestroy, StagePostDestroy, StageCleanup}; !slices.Equal(destroyOrder.Order, want) {
		t.Fatalf("GetDestroyStageOrder() order = %v, want %v", destroyOrder.Order, want)
	}

	for _, stageName := range destroyOrder.Order {
		if destroyOrder.Funcs[stageName] == nil {
			t.Fatalf("GetDestroyStageOrder() missing function for %q", stageName)
		}
	}
}
